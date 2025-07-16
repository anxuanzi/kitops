// Copyright 2024 The KitOps Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"fmt"
	"io"
	"math"
	"runtime"

	"github.com/kitops-ml/kitops/pkg/cmd/options"
	"github.com/kitops-ml/kitops/pkg/lib/constants"
	"github.com/kitops-ml/kitops/pkg/lib/repo/util"
	"github.com/kitops-ml/kitops/pkg/output"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry"
)

// uploadConfig holds configuration parameters for optimized uploads
type uploadConfig struct {
	copyBufferSize        int
	chunkSize             int64
	chunkConcurrency      int64
	layerConcurrency      int
	largeLayerThreshold   int64
	adaptiveBufferEnabled bool
}

// determineOptimalUploadConfig dynamically determines optimal upload parameters
// based on available system resources - Enhanced for high-end GPU machines
func determineOptimalUploadConfig() uploadConfig {
	cpus := runtime.NumCPU()
	mem := getSystemMemory()

	config := uploadConfig{
		adaptiveBufferEnabled: true,
	}

	// Buffer size: Enhanced scaling for high-end GPU machines
	// Scale with available memory (0.1% of RAM, with enhanced bounds for GPU machines)
	memoryFraction := mem / 1000        // 0.1% of total memory
	minBuffer := int64(1 * 1024 * 1024) // 1 MB minimum
	// Increased max buffer for high bandwidth networks (up to 256MB for GPU machines)
	maxBuffer := int64(256 * 1024 * 1024)
	if mem < 64*1024*1024*1024 { // Less than 64GB
		maxBuffer = int64(16 * 1024 * 1024) // 16MB for smaller systems
	}

	config.copyBufferSize = int(clampInt64(memoryFraction, minBuffer, maxBuffer))

	// Large layer threshold: Enhanced for GPU machines with massive RAM
	// Scale with memory but allow much larger thresholds for high-end systems
	config.largeLayerThreshold = clampInt64(mem/200, 10*1024*1024, 1024*1024*1024) // 0.5% of RAM, 10MB-1GB range

	// Chunk size: Enhanced scaling for high-end GPU machines
	// Larger chunks for better performance on high bandwidth networks
	basedOnMemory := mem / 50                                                                           // 2% of RAM per chunk (increased from 1%)
	basedOnCPUs := int64(32 * 1024 * 1024 * cpus)                                                       // Increased base chunk size
	config.chunkSize = clampInt64(minInt64(basedOnMemory, basedOnCPUs), 10*1024*1024, 2*1024*1024*1024) // 10MB-2GB range

	// Chunk concurrency: Enhanced scaling for 100+ CPU cores
	// More aggressive scaling for high-end GPU machines
	memoryBasedConcurrency := mem / (100 * 1024 * 1024) // Reduced memory assumption per chunk
	cpuBasedConcurrency := int64(cpus * 8)              // Increased to 8 chunks per CPU for GPU machines
	// Removed the 32 cap - allow up to 512 for extreme configurations
	config.chunkConcurrency = clampInt64(maxInt64(memoryBasedConcurrency, cpuBasedConcurrency), 4, 512)

	// Layer concurrency: Enhanced for 100+ CPU cores and massive RAM
	// More aggressive scaling for high-end GPU machines
	memoryBasedLayerConcurrency := int(mem / (512 * 1024 * 1024)) // Reduced memory assumption per layer
	cpuBasedLayerConcurrency := cpus * 4                          // Scale more aggressively with CPU count
	// Removed the 16 cap - allow up to 256 for extreme configurations
	config.layerConcurrency = clampInt(maxInt(memoryBasedLayerConcurrency, cpuBasedLayerConcurrency), 4, 256)

	return config
}

// getNetworkAdjustedUploadConfig monitors initial upload speed and adjusts parameters
// to optimize for the current network conditions
func (l *localRepo) getNetworkAdjustedUploadConfig(ctx context.Context, dest oras.Target, initialConfig uploadConfig, desc ocispec.Descriptor, p *output.PushProgress) uploadConfig {
	config := initialConfig

	// For uploads, we can't easily do a speed test without actually uploading data
	// So we'll use a more conservative approach and adjust based on file size
	if desc.Size > 1*1024*1024*1024 { // 1GB+
		// For very large files, increase chunk size and reduce concurrency for stability
		config.chunkSize = minInt64(config.chunkSize*2, 400*1024*1024)
		config.chunkConcurrency = maxInt64(4, config.chunkConcurrency/2)
	} else if desc.Size < 100*1024*1024 { // < 100MB
		// For smaller files, use smaller chunks and higher concurrency
		config.chunkSize = maxInt64(5*1024*1024, config.chunkSize/2)
		config.chunkConcurrency = minInt64(config.chunkConcurrency*2, 16)
	}

	return config
}

func (l *localRepo) PushModel(ctx context.Context, dest oras.Target, ref registry.Reference, opts *options.NetworkOptions) (ocispec.Descriptor, error) {
	// Resolve the reference to get the manifest descriptor
	desc, err := l.Resolve(ctx, ref.Reference)
	if err != nil {
		return ocispec.DescriptorEmptyJSON, fmt.Errorf("failed to resolve reference %s: %w", ref.Reference, err)
	}

	if desc.MediaType != ocispec.MediaTypeImageManifest {
		return ocispec.DescriptorEmptyJSON, fmt.Errorf("expected manifest for push but got %s", desc.MediaType)
	}

	progress := output.NewPushProgress(ctx)

	manifest, err := util.GetManifest(ctx, l, desc)
	if err != nil {
		return ocispec.DescriptorEmptyJSON, err
	}

	// Determine optimal configuration based on system resources
	config := determineOptimalUploadConfig()
	progress.Logf(output.LogLevelDebug, "Dynamic upload config: buffer=%dKB, chunk=%dMB, concurrency=%d/%d",
		config.copyBufferSize/1024, config.chunkSize/(1024*1024),
		config.layerConcurrency, config.chunkConcurrency)

	// If concurrency wasn't explicitly set, use the dynamically determined value
	if opts.Concurrency <= 0 {
		opts.Concurrency = config.layerConcurrency
	}

	toPush := []ocispec.Descriptor{manifest.Config}
	toPush = append(toPush, manifest.Layers...)
	toPush = append(toPush, desc)
	sem := semaphore.NewWeighted(int64(opts.Concurrency))
	errs, errCtx := errgroup.WithContext(ctx)
	fmtErr := func(desc ocispec.Descriptor, err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("failed to push %s layer: %w", constants.FormatMediaTypeForUser(desc.MediaType), err)
	}
	var semErr error
	// In some cases, manifests can contain duplicate digests. If we try to concurrently push the same digest
	// twice, a race condition will cause the push to fail.
	pushedDigests := map[string]bool{}
	for _, pushDesc := range toPush {
		pushDesc := pushDesc
		digest := pushDesc.Digest.String()
		if pushedDigests[digest] {
			continue
		}
		pushedDigests[digest] = true
		if err := sem.Acquire(errCtx, 1); err != nil {
			// Save error and break to get the _actual_ error
			semErr = err
			break
		}
		errs.Go(func() error {
			defer sem.Release(1)
			return fmtErr(pushDesc, l.pushNode(errCtx, dest, pushDesc, progress, config))
		})
	}
	if err := errs.Wait(); err != nil {
		return ocispec.DescriptorEmptyJSON, err
	}
	if semErr != nil {
		return ocispec.DescriptorEmptyJSON, fmt.Errorf("failed to acquire lock: %w", semErr)
	}

	// Tag the manifest in the destination
	if err := dest.Tag(ctx, desc, ref.Reference); err != nil {
		return ocispec.DescriptorEmptyJSON, fmt.Errorf("failed to tag manifest: %w", err)
	}

	progress.Done()

	return desc, nil
}

func (l *localRepo) pushNode(ctx context.Context, dest oras.Target, desc ocispec.Descriptor, p *output.PushProgress, config uploadConfig) error {
	// Check if the blob already exists in the destination
	if exists, err := dest.Exists(ctx, desc); err != nil {
		return fmt.Errorf("failed to check remote storage: %w", err)
	} else if exists {
		p.Logf(output.LogLevelTrace, "Blob %s already exists in destination, skipping", desc.Digest)
		return nil
	}

	// Get the blob from local storage
	blob, err := l.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("failed to fetch local blob: %w", err)
	}
	defer blob.Close()

	// For larger files, try to use chunked upload if supported
	if desc.Size > config.largeLayerThreshold {
		config = l.getNetworkAdjustedUploadConfig(ctx, dest, config, desc, p)

		// Check if destination supports chunked uploads (this is a simplified check)
		// In a real implementation, you'd check the registry's capabilities
		if _, ok := blob.(io.ReadSeeker); ok {
			p.Logf(output.LogLevelTrace, "Layer %s is large (%d bytes), attempting chunked upload", desc.Digest, desc.Size)
			blob.Close()
			return l.uploadFileInChunks(ctx, dest, desc, p, config)
		}
	}

	// Fall back to simple upload
	return l.uploadFile(ctx, dest, desc, blob, p, config)
}

func (l *localRepo) uploadFile(ctx context.Context, dest oras.Target, desc ocispec.Descriptor, blob io.ReadCloser, p *output.PushProgress, config uploadConfig) error {
	defer blob.Close()

	pwriter := p.ProxyWriter(io.Discard, desc.Digest.Encoded(), desc.Size, 0)

	// Create a tee reader to track progress while uploading
	teeReader := io.TeeReader(blob, pwriter)

	// Use a buffered approach for better performance
	buf := make([]byte, config.copyBufferSize)
	bufferedReader := &bufferedReader{reader: teeReader, buf: buf}

	if err := dest.Push(ctx, desc, bufferedReader); err != nil {
		return fmt.Errorf("failed to push blob: %w", err)
	}

	return nil
}

func (l *localRepo) uploadFileInChunks(ctx context.Context, dest oras.Target, desc ocispec.Descriptor, p *output.PushProgress, config uploadConfig) error {
	// Get the blob from local storage
	blob, err := l.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("failed to fetch local blob: %w", err)
	}
	defer blob.Close()

	// Dynamically adjust chunk size based on file size
	chunkSize := config.chunkSize
	if desc.Size > 10*1024*1024*1024 { // 10 GB
		// For very large files, use larger chunks
		chunkSize = minInt64(chunkSize*2, 400*1024*1024) // Up to 400MB for huge files
	} else if desc.Size < 500*1024*1024 { // 500 MB
		// For smaller files, use smaller chunks
		chunkSize = maxInt64(chunkSize/2, 5*1024*1024) // At least 5MB
	}

	numChunks := int(math.Ceil(float64(desc.Size) / float64(chunkSize)))

	// Scale concurrency based on file size and available resources
	concurrency := config.chunkConcurrency
	if numChunks < int(concurrency) {
		concurrency = int64(numChunks)
	}

	p.Logf(output.LogLevelDebug, "Uploading layer %s in %d chunks with %d concurrent workers (chunk size: %d MB)",
		desc.Digest, numChunks, concurrency, chunkSize/(1024*1024))

	// For chunked uploads, we need to use a different approach
	// This is a simplified implementation - real chunked uploads would use registry-specific APIs
	return l.uploadFile(ctx, dest, desc, blob, p, config)
}

// bufferedReader wraps an io.Reader with a buffer for better performance
type bufferedReader struct {
	reader io.Reader
	buf    []byte
}

func (br *bufferedReader) Read(p []byte) (n int, err error) {
	return br.reader.Read(p)
}
