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
	"errors"
	"fmt"
	"github.com/kitops-ml/kitops/pkg/cmd/options"
	"github.com/kitops-ml/kitops/pkg/lib/constants"
	"github.com/kitops-ml/kitops/pkg/lib/repo/util"
	"github.com/kitops-ml/kitops/pkg/output"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry"
)

// downloadConfig holds all dynamically determined download configuration parameters
type downloadConfig struct {
	copyBufferSize        int
	largeLayerThreshold   int64
	chunkSize             int64
	chunkConcurrency      int64
	layerConcurrency      int
	adaptiveBufferEnabled bool
}

// getSystemMemory returns the total system memory in bytes using cross-platform approach
// Enhanced for high-end GPU machines with 1000G+ RAM
func getSystemMemory() int64 {
	// Try to get memory info from runtime stats
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// If we can get system memory info, use a more sophisticated estimate
	// that can handle high-end GPU machines with massive RAM
	if m.Sys > 0 {
		// For high-end systems, use a more aggressive multiplier
		// GPU machines often have much more system RAM than heap usage suggests
		estimatedTotal := int64(m.Sys) * 16 // Increased multiplier for GPU machines

		// Enhanced bounds for high-end GPU machines (1GB to 2TB)
		if estimatedTotal < 1*1024*1024*1024 {
			estimatedTotal = 8 * 1024 * 1024 * 1024 // Default to 8GB
		}
		// Removed the 128GB cap - allow up to 2TB for high-end GPU machines
		if estimatedTotal > 2*1024*1024*1024*1024 {
			estimatedTotal = 2 * 1024 * 1024 * 1024 * 1024 // Cap at 2TB
		}

		return estimatedTotal
	}

	// Fallback default - increased for modern systems
	return 16 * 1024 * 1024 * 1024 // 16 GB default
}

// determineOptimalConfig dynamically determines optimal download parameters
// based on available system resources - Enhanced for high-end GPU machines
func determineOptimalConfig() downloadConfig {
	cpus := runtime.NumCPU()
	mem := getSystemMemory()

	// Basic heuristics - actual values will be adjusted based on file size and network conditions
	config := downloadConfig{
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

// getNetworkAdjustedConfig monitors initial download speed and adjusts parameters
// to optimize for the current network conditions
func (l *localRepo) getNetworkAdjustedConfig(ctx context.Context, src oras.ReadOnlyTarget, initialConfig downloadConfig, desc ocispec.Descriptor, p *output.PullProgress) downloadConfig {
	config := initialConfig

	// Create a test download to measure network speed
	start := time.Now()
	testSize := int64(10 * 1024 * 1024) // 10MB test download for more accurate measurement

	// Only run the test if the file is large enough
	if desc.Size <= testSize*2 {
		return config // File too small to bother with network testing
	}

	// Get a small sample to measure network speed
	rc, err := l.fetchAndSeek(ctx, src, desc, 0, testSize)
	if err != nil {
		p.Logf(output.LogLevelDebug, "Skipping network speed test: %v", err)
		return config
	}
	defer rc.Close()

	// Discard the data, we just care about speed
	buf := make([]byte, 32*1024)
	n, err := io.CopyBuffer(io.Discard, rc, buf)
	elapsed := time.Since(start)

	if err != nil || n < testSize/2 || elapsed > 5*time.Second {
		// Network seems slow or unstable
		p.Logf(output.LogLevelDebug, "Network appears slow or unstable, optimizing for reliability")
		// Reduce concurrency and chunk size for more reliable downloads
		config.chunkConcurrency = maxInt64(4, config.chunkConcurrency/2)
		config.layerConcurrency = maxInt(2, config.layerConcurrency/2)
		config.chunkSize = maxInt64(5*1024*1024, config.chunkSize/2)
		return config
	}

	// Calculate speed in MB/s
	mbps := float64(n) / (1024 * 1024) / elapsed.Seconds()
	p.Logf(output.LogLevelDebug, "Network speed test: %.2f MB/s", mbps)

	// Adjust concurrency dynamically based on measured speed.
	// Use 20 MB/s as a baseline for scaling. Speeds significantly higher
	// than this will increase concurrency proportionally, while slower
	// connections will decrease it a bit to reduce overhead.
	baseline := 20.0
	factor := mbps / baseline

	if factor > 1 {
		scale := math.Min(factor, 8) // don't grow unbounded
		config.chunkConcurrency = minInt64(int64(float64(config.chunkConcurrency)*scale), 128)
		config.layerConcurrency = minInt(int(float64(config.layerConcurrency)*scale), 64)
		config.chunkSize = minInt64(int64(float64(config.chunkSize)*scale), 400*1024*1024)
	} else if factor < 0.5 {
		scale := math.Max(factor, 0.25)
		config.chunkConcurrency = maxInt64(2, int64(float64(config.chunkConcurrency)*scale))
		config.layerConcurrency = maxInt(1, int(float64(config.layerConcurrency)*scale))
		config.chunkSize = maxInt64(5*1024*1024, int64(float64(config.chunkSize)*scale))
	}

	return config
}

// Helper function to fetch and seek a remote resource
func (l *localRepo) fetchAndSeek(ctx context.Context, src oras.ReadOnlyTarget, desc ocispec.Descriptor, offset, length int64) (io.ReadCloser, error) {
	rc, err := src.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}

	seeker, ok := rc.(io.ReadSeeker)
	if !ok {
		rc.Close()
		return nil, fmt.Errorf("remote does not support seeking")
	}

	if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
		rc.Close()
		return nil, err
	}

	return io.NopCloser(io.LimitReader(rc, length)), nil
}

func (l *localRepo) PullModel(ctx context.Context, src oras.ReadOnlyTarget, ref registry.Reference, opts *options.NetworkOptions) (ocispec.Descriptor, error) {
	// Only support pulling image manifests
	desc, err := src.Resolve(ctx, ref.Reference)
	if err != nil {
		return ocispec.DescriptorEmptyJSON, err
	}
	if desc.MediaType != ocispec.MediaTypeImageManifest {
		return ocispec.DescriptorEmptyJSON, fmt.Errorf("expected manifest for pull but got %s", desc.MediaType)
	}

	if err := l.ensurePullDirs(); err != nil {
		return ocispec.DescriptorEmptyJSON, fmt.Errorf("failed to set up directories for pull: %w", err)
	}

	progress := output.NewPullProgress(ctx)

	manifest, err := util.GetManifest(ctx, src, desc)
	if err != nil {
		return ocispec.DescriptorEmptyJSON, err
	}

	// Determine optimal configuration based on system resources
	config := determineOptimalConfig()
	progress.Logf(output.LogLevelDebug, "Dynamic config: buffer=%dKB, chunk=%dMB, concurrency=%d/%d",
		config.copyBufferSize/1024, config.chunkSize/(1024*1024),
		config.layerConcurrency, config.chunkConcurrency)

	// If concurrency wasn't explicitly set, use the dynamically determined value
	if opts.Concurrency <= 0 {
		opts.Concurrency = config.layerConcurrency
	}

	toPull := []ocispec.Descriptor{manifest.Config}
	toPull = append(toPull, manifest.Layers...)
	toPull = append(toPull, desc)

	// Implement adaptive concurrency: separate files by size for optimal resource utilization
	if err := l.pullWithAdaptiveConcurrency(ctx, src, toPull, progress, config, opts.Concurrency); err != nil {
		return ocispec.DescriptorEmptyJSON, err
	}

	// Special handling to make sure local (scoped) repo contains the just-pulled manifest
	if err := l.localIndex.addManifest(desc); err != nil {
		return ocispec.DescriptorEmptyJSON, fmt.Errorf("failed to add manifest to index: %w", err)
	}
	// This is a workaround to add the manifest to the main index as well; this is necessary for garbage collection to work
	if err := l.Store.Tag(ctx, desc, desc.Digest.String()); err != nil {
		return ocispec.DescriptorEmptyJSON, fmt.Errorf("failed to add manifest to shared index: %w", err)
	}

	if !util.ReferenceIsDigest(ref.Reference) {
		if err := l.localIndex.tag(desc, ref.Reference); err != nil {
			return ocispec.DescriptorEmptyJSON, fmt.Errorf("failed to save tag: %w", err)
		}
	}
	progress.Done()

	if err := l.cleanupIngestDir(); err != nil {
		output.Logln(output.LogLevelWarn, err)
	}

	return desc, nil
}

// pullWithAdaptiveConcurrency implements adaptive concurrency strategy:
// - Large files are downloaded sequentially to maximize bandwidth utilization per file
// - Small files are downloaded concurrently for better overall throughput
func (l *localRepo) pullWithAdaptiveConcurrency(ctx context.Context, src oras.ReadOnlyTarget, toPull []ocispec.Descriptor, progress *output.PullProgress, config downloadConfig, maxConcurrency int) error {
	// Separate files into small and large groups based on largeLayerThreshold
	var smallFiles, largeFiles []ocispec.Descriptor

	// Remove duplicates first to avoid race conditions
	pulledDigests := map[string]bool{}
	uniqueFiles := make([]ocispec.Descriptor, 0, len(toPull))
	for _, desc := range toPull {
		digest := desc.Digest.String()
		if !pulledDigests[digest] {
			pulledDigests[digest] = true
			uniqueFiles = append(uniqueFiles, desc)
		}
	}

	// Categorize files by size
	for _, desc := range uniqueFiles {
		if desc.Size > config.largeLayerThreshold {
			largeFiles = append(largeFiles, desc)
		} else {
			smallFiles = append(smallFiles, desc)
		}
	}

	progress.Logf(output.LogLevelDebug, "Adaptive concurrency: %d large files (sequential), %d small files (concurrent=%d)",
		len(largeFiles), len(smallFiles), maxConcurrency)

	// Initialize all layer progress bars upfront (Docker CLI-like behavior)
	progress.InitializeLayers(uniqueFiles)

	// Create error group for coordinating all downloads
	errs, errCtx := errgroup.WithContext(ctx)

	fmtErr := func(desc ocispec.Descriptor, err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("failed to get %s layer: %w", constants.FormatMediaTypeForUser(desc.MediaType), err)
	}

	// Process large files sequentially (concurrency = 1) to maximize bandwidth per file
	// This ensures each large file gets full network bandwidth utilization
	if len(largeFiles) > 0 {
		errs.Go(func() error {
			progress.Logf(output.LogLevelDebug, "Processing %d large files sequentially", len(largeFiles))
			for _, desc := range largeFiles {
				desc := desc // capture loop variable
				if err := l.pullNode(errCtx, src, desc, progress, config); err != nil {
					return fmtErr(desc, err)
				}
			}
			return nil
		})
	}

	// Process small files concurrently for better throughput
	// Small files benefit from concurrent processing as they don't saturate bandwidth individually
	if len(smallFiles) > 0 {
		errs.Go(func() error {
			progress.Logf(output.LogLevelDebug, "Processing %d small files with concurrency=%d", len(smallFiles), maxConcurrency)

			sem := semaphore.NewWeighted(int64(maxConcurrency))
			smallErrs, smallCtx := errgroup.WithContext(errCtx)

			for _, desc := range smallFiles {
				desc := desc // capture loop variable
				if err := sem.Acquire(smallCtx, 1); err != nil {
					return err
				}
				smallErrs.Go(func() error {
					defer sem.Release(1)
					return fmtErr(desc, l.pullNode(smallCtx, src, desc, progress, config))
				})
			}

			return smallErrs.Wait()
		})
	}

	return errs.Wait()
}

func (l *localRepo) pullNode(ctx context.Context, src oras.ReadOnlyTarget, desc ocispec.Descriptor, p *output.PullProgress, config downloadConfig) error {
	if exists, err := l.Exists(ctx, desc); err != nil {
		return fmt.Errorf("failed to check local storage: %w", err)
	} else if exists {
		return nil
	}

	// For larger files, try to adjust configuration based on a quick network speed test
	if desc.Size > config.largeLayerThreshold {
		config = l.getNetworkAdjustedConfig(ctx, src, config, desc, p)
	}

	blob, err := src.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("failed to fetch: %w", err)
	}

	if seekBlob, ok := blob.(io.ReadSeekCloser); ok {
		// For large layers where the remote supports seeking (which implies support for
		// HTTP Range requests), we use a new parallel chunking strategy to speed up
		// the download of the single layer.
		if desc.Size > config.largeLayerThreshold {
			p.Logf(output.LogLevelTrace, "Layer %s is large (%d bytes), using parallel chunk download", desc.Digest, desc.Size)
			// Close the initially fetched blob; the chunking function will manage its own fetches.
			seekBlob.Close()
			return l.downloadFileInChunks(ctx, src, desc, p, config)
		}

		// For smaller layers that support seeking, continue with the original resumable download logic.
		p.Logf(output.LogLevelTrace, "Remote supports range requests, using resumable download")
		return l.resumeAndDownloadFile(desc, seekBlob, p, config)
	} else {
		// If the remote does not support seeking, fall back to a simple, non-resumable download.
		return l.downloadFile(desc, blob, p, config)
	}
}

func (l *localRepo) resumeAndDownloadFile(desc ocispec.Descriptor, blob io.ReadSeekCloser, p *output.PullProgress, config downloadConfig) error {
	ingestDir := constants.IngestPath(l.storagePath)
	ingestFilename := filepath.Join(ingestDir, desc.Digest.Encoded())
	ingestFile, err := os.OpenFile(ingestFilename, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open ingest file for writing: %w", err)
	}
	defer func() {
		if err := ingestFile.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
			p.Logf(output.LogLevelError, "Error closing temporary ingest file: %s", err)
		}
	}()

	verifier := desc.Digest.Verifier()
	var offset int64 = 0
	if stat, err := ingestFile.Stat(); err != nil {
		return fmt.Errorf("failed to stat ingest file: %w", err)
	} else if stat.Size() != 0 {
		p.Debugf("Resuming download for digest %s", desc.Digest.String())
		numBytes, err := io.Copy(verifier, ingestFile)
		if err != nil {
			return fmt.Errorf("failed to resume download: %w", err)
		}
		p.Logf(output.LogLevelTrace, "Updating offset to %d bytes", numBytes)
		offset = numBytes
	}
	if _, err := blob.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek in remote resource: %w", err)
	}

	pwriter := p.ProxyWriter(ingestFile, desc.Digest.Encoded(), desc.Size, offset)
	mw := io.MultiWriter(pwriter, verifier)

	// Use io.CopyBuffer with a dynamically sized buffer
	buf := make([]byte, config.copyBufferSize)
	if _, err := io.CopyBuffer(mw, blob, buf); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	if !verifier.Verified() {
		return fmt.Errorf("downloaded file hash does not match descriptor")
	}
	if err := ingestFile.Close(); err != nil {
		return fmt.Errorf("failed to close temporary ingest file: %w", err)
	}
	blobPath := l.BlobPath(desc)
	if err := os.Rename(ingestFilename, blobPath); err != nil {
		return fmt.Errorf("failed to move downloaded file into storage: %w", err)
	}
	if err := os.Chmod(blobPath, 0600); err != nil {
		return fmt.Errorf("failed to set permissions on blob: %w", err)
	}

	return nil
}

func (l *localRepo) downloadFile(desc ocispec.Descriptor, blob io.ReadCloser, p *output.PullProgress, config downloadConfig) (ingestErr error) {
	ingestDir := constants.IngestPath(l.storagePath)
	ingestFile, err := os.CreateTemp(ingestDir, desc.Digest.Encoded()+"_*")
	if err != nil {
		return fmt.Errorf("failed to create temporary ingest file: %w", err)
	}

	ingestFilename := ingestFile.Name()
	// If we return an error anywhere after this point, we want to delete the ingest file we're
	// working on, since it will never be reused.
	defer func() {
		if err := ingestFile.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
			p.Logf(output.LogLevelError, "Error closing temporary ingest file: %s", err)
		}
		if ingestErr != nil {
			os.Remove(ingestFilename)
		}
	}()

	verifier := desc.Digest.Verifier()
	pwriter := p.ProxyWriter(ingestFile, desc.Digest.Encoded(), desc.Size, 0)
	mw := io.MultiWriter(pwriter, verifier)

	// Use io.CopyBuffer with dynamically sized buffer
	buf := make([]byte, config.copyBufferSize)
	if _, err := io.CopyBuffer(mw, blob, buf); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	if !verifier.Verified() {
		return fmt.Errorf("downloaded file hash does not match descriptor")
	}
	if err := ingestFile.Close(); err != nil {
		return fmt.Errorf("failed to close temporary ingest file: %w", err)
	}

	blobPath := l.BlobPath(desc)
	if err := os.Rename(ingestFilename, blobPath); err != nil {
		return fmt.Errorf("failed to move downloaded file into storage: %w", err)
	}
	if err := os.Chmod(blobPath, 0600); err != nil {
		return fmt.Errorf("failed to set permissions on blob: %w", err)
	}

	return nil
}

func (l *localRepo) ensurePullDirs() error {
	blobsPath := filepath.Join(l.storagePath, ocispec.ImageBlobsDir, "sha256")
	if err := os.MkdirAll(blobsPath, 0755); err != nil {
		return err
	}
	ingestPath := constants.IngestPath(l.storagePath)
	return os.MkdirAll(ingestPath, 0755)
}

func (l *localRepo) cleanupIngestDir() error {
	ingestPath := constants.IngestPath(l.storagePath)
	err := filepath.WalkDir(ingestPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to clean up ingest directory: %w", err)
	}
	return nil
}

// offsetWriter is a helper that implements io.Writer but writes at a specific offset.
type offsetWriter struct {
	w      io.WriterAt
	offset int64
}

func (ow *offsetWriter) Write(p []byte) (n int, err error) {
	n, err = ow.w.WriteAt(p, ow.offset)
	ow.offset += int64(n)
	return
}

// downloadFileInChunks implements a parallel download strategy for large files. It splits
// the file into chunks and downloads them concurrently. This is particularly effective
// for maximizing bandwidth utilization on high-speed networks.
func (l *localRepo) downloadFileInChunks(ctx context.Context, src oras.ReadOnlyTarget, desc ocispec.Descriptor, p *output.PullProgress, config downloadConfig) (err error) {
	ingestDir := constants.IngestPath(l.storagePath)
	ingestFile, err := os.CreateTemp(ingestDir, desc.Digest.Encoded()+"_chunked_*")
	if err != nil {
		return fmt.Errorf("failed to create temporary ingest file: %w", err)
	}
	ingestFilename := ingestFile.Name()
	defer func() {
		ingestFile.Close()
		if err != nil {
			os.Remove(ingestFilename)
		}
	}()

	// Pre-allocate the file to its full size
	if err := ingestFile.Truncate(desc.Size); err != nil {
		return fmt.Errorf("failed to pre-allocate file space: %w", err)
	}

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

	p.Logf(output.LogLevelDebug, "Downloading layer %s in %d chunks with %d concurrent workers (chunk size: %d MB)",
		desc.Digest, numChunks, concurrency, chunkSize/(1024*1024))

	g, gCtx := errgroup.WithContext(ctx)
	sem := semaphore.NewWeighted(concurrency)

	// This ProxyWriter will be used concurrently to report progress
	pwriter := p.ProxyWriter(io.Discard, desc.Digest.Encoded(), desc.Size, 0)

	// Create a channel to monitor download speeds of the first few chunks
	speedChan := make(chan float64, 5)
	adaptiveConfig := config.adaptiveBufferEnabled && numChunks > 5

	for i := 0; i < numChunks; i++ {
		if err := sem.Acquire(gCtx, 1); err != nil {
			break // Context was cancelled
		}

		chunkIndex := i
		g.Go(func() error {
			defer sem.Release(1)

			start := int64(chunkIndex) * chunkSize
			length := chunkSize
			if start+length > desc.Size {
				length = desc.Size - start
			}

			// Time this chunk download for adaptive configuration
			chunkStart := time.Now()

			// Fetch a new reader for each chunk
			rc, fetchErr := src.Fetch(gCtx, desc)
			if fetchErr != nil {
				return fmt.Errorf("chunk %d: failed to fetch: %w", chunkIndex, fetchErr)
			}
			defer rc.Close()

			seeker, ok := rc.(io.ReadSeeker)
			if !ok {
				return fmt.Errorf("chunk %d: remote does not support seek, cannot download in chunks", chunkIndex)
			}
			if _, seekErr := seeker.Seek(start, io.SeekStart); seekErr != nil {
				return fmt.Errorf("chunk %d: failed to seek to offset %d: %w", chunkIndex, start, seekErr)
			}

			// Use a LimitedReader to ensure we don't read past the chunk boundary
			limitedReader := io.LimitReader(rc, length)

			// Determine buffer size - may be dynamically adjusted based on early chunk performance
			bufSize := config.copyBufferSize
			if adaptiveConfig && chunkIndex > 3 && len(speedChan) > 0 {
				// Calculate average speed from first chunks
				var totalSpeed float64
				var count int
				for len(speedChan) > 0 && count < 3 {
					totalSpeed += <-speedChan
					count++
				}

				if count > 0 {
					avgSpeed := totalSpeed / float64(count)

					// Adjust buffer size based on observed speed
					if avgSpeed > 20*1024*1024 { // 20 MB/s
						// Fast connection - use larger buffers
						bufSize = minInt(bufSize*2, 32*1024*1024) // Up to 32MB
					} else if avgSpeed < 1*1024*1024 { // 1 MB/s
						// Slow connection - use smaller buffers
						bufSize = maxInt(bufSize/2, 32*1024) // At least 32KB
					}
				}
			}

			buf := make([]byte, bufSize)
			n, err := io.CopyBuffer(
				io.MultiWriter(pwriter, &offsetWriter{w: ingestFile, offset: start}),
				limitedReader,
				buf,
			)

			// If this is one of the first few chunks, report its speed for adaptive configuration
			if adaptiveConfig && chunkIndex < 3 && err == nil && n > 0 {
				elapsed := time.Since(chunkStart).Seconds()
				if elapsed > 0 {
					speed := float64(n) / elapsed // bytes per second
					select {
					case speedChan <- speed:
					default:
						// Channel full, just continue
					}
				}
			}

			if err != nil {
				return fmt.Errorf("chunk %d: failed to write to file: %w", chunkIndex, err)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	// Verify the integrity of the complete file
	if _, err := ingestFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek to start of ingest file for verification: %w", err)
	}
	verifier := desc.Digest.Verifier()
	if _, err := io.Copy(verifier, ingestFile); err != nil {
		return fmt.Errorf("failed to verify downloaded file: %w", err)
	}
	if !verifier.Verified() {
		return fmt.Errorf("downloaded file hash does not match descriptor")
	}

	if err := ingestFile.Close(); err != nil {
		return fmt.Errorf("failed to close temporary ingest file: %w", err)
	}
	blobPath := l.BlobPath(desc)
	if err := os.Rename(ingestFilename, blobPath); err != nil {
		return fmt.Errorf("failed to move downloaded file into storage: %w", err)
	}
	if err := os.Chmod(blobPath, 0600); err != nil {
		return fmt.Errorf("failed to set permissions on blob: %w", err)
	}
	return nil
}

// Helper functions for min/max and clamping values - fixed to avoid conflicts
func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func clampInt64(value, min, max int64) int64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// Integer versions
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
