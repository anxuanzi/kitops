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

package remote

import (
	"bytes"
	"context"
	"fmt"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"

	"github.com/kitops-ml/kitops/pkg/output"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// uploadChunkConcurrency is the number of chunks to upload in parallel. It is
// dynamically determined based on available CPU resources, but can be
// overridden via the KITOPS_UPLOAD_CONCURRENCY environment variable.
var uploadChunkConcurrency = defaultUploadConcurrency()

type Repository struct {
	registry.Repository
	Reference registry.Reference
	PlainHttp bool
	Client    remote.Client
}

// Push pushes the content, matching the expected descriptor.
func (r *Repository) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	if expected.MediaType == ocispec.MediaTypeImageManifest {
		// If it's a manifest, we can just use the regular implementation
		return r.Repository.Push(ctx, expected, content)
	}

	// Otherwise, push a blob according to the OCI spec
	ctx = auth.AppendRepositoryScope(ctx, r.Reference, auth.ActionPull, auth.ActionPush)
	sessionURL, postResp, err := r.initiateUploadSession(ctx)
	if err != nil {
		return err
	}

	blobUrl, err := r.uploadBlob(ctx, sessionURL, postResp, expected, content)
	if err != nil {
		return err
	}
	output.SafeDebugf("Blob uploaded, available at url %s", blobUrl)

	return nil
}

func (r *Repository) initiateUploadSession(ctx context.Context) (*url.URL, *http.Response, error) {
	uploadUrl := buildRepositoryBlobUploadURL(r.PlainHttp, r.Reference)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadUrl, nil)
	if err != nil {
		return nil, nil, err
	}

	// TODO: Handle warnings from remote
	// References:
	//   - https://github.com/opencontainers/distribution-spec/blob/v1.1.0-rc4/spec.md#warnings
	//   - https://www.rfc-editor.org/rfc/rfc7234#section-5.5
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initiate upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return nil, nil, handleRemoteError(resp)
	}
	location, err := resp.Location()
	if err != nil {
		return nil, nil, fmt.Errorf("registry did not respond with upload location")
	}

	// Workaround for https://github.com/oras-project/oras-go/issues/177 -- sometimes
	// location header does not include port, causing auth client to mismatch the context
	locationHostname := location.Hostname()
	locationPort := location.Port()
	origHostname := req.URL.Hostname()
	origPort := req.URL.Port()
	if origPort == "443" && locationHostname == origHostname && locationPort == "" {
		location.Host = locationHostname + ":" + origPort
	}
	output.SafeDebugf("Using location %s for blob upload", path.Join(location.Hostname(), location.Path))

	return location, resp, nil
}

// uploadBlob now checks if the content reader supports random access (io.ReaderAt).
// If it does, it uses the new parallel chunked upload strategy. Otherwise, it falls
// back to the original sequential method.
func (r *Repository) uploadBlob(ctx context.Context, location *url.URL, postResp *http.Response, expected ocispec.Descriptor, content io.Reader) (string, error) {
	output.SafeDebugf("Size: %d", expected.Size)
	uploadFormat := getUploadFormat(location.Hostname(), expected.Size)
	switch uploadFormat {
	case uploadMonolithicPut:
		return r.uploadBlobMonolithic(ctx, location, postResp, expected, content)
	case uploadChunkedPatch:
		// --- NEW ---
		// Check if the reader supports io.ReaderAt for parallel uploads.
		// os.File implements this, so pushing from a file will be parallelized.
		if readerAt, ok := content.(io.ReaderAt); ok {
			output.SafeDebugf("Content supports random access, using parallel chunked upload.")
			return r.uploadBlobChunkedParallel(ctx, location, postResp, expected, readerAt)
		}
		// Fallback for simple streams that don't support random access.
		output.SafeDebugf("Content is a stream, using sequential chunked upload.")
		return r.uploadBlobChunkedSequential(ctx, location, postResp, expected, content)
	default:
		return "", fmt.Errorf("unknown registry %s, cannot upload", location.Hostname())
	}
}

// uploadBlobMonolithic performs a monolithic blob upload as per the distribution spec. The content of the blob is uploaded
// in one PUT request at the provided location.
func (r *Repository) uploadBlobMonolithic(ctx context.Context, location *url.URL, postResp *http.Response, expected ocispec.Descriptor, content io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, location.String(), content)
	if err != nil {
		return "", err
	}
	// Set Content-Length header
	if req.GetBody != nil && req.ContentLength != expected.Size {
		// short circuit a size mismatch for built-in types.
		return "", fmt.Errorf("mismatch content length %d: expect %d", req.ContentLength, expected.Size)
	}
	req.ContentLength = expected.Size

	// Set Content-Type to required 'application/octet-stream'
	req.Header.Set("Content-Type", "application/octet-stream")

	// Set digest query to mark this as completing the upload
	q := req.URL.Query()
	q.Set("digest", expected.Digest.String())
	req.URL.RawQuery = q.Encode()

	// Reuse credentials from POST request that initiated upload
	if auth := postResp.Request.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	output.SafeDebugf("Uploading blob as one chunk")
	// TODO: Handle warnings from remote
	// References:
	//   - https://github.com/opencontainers/distribution-spec/blob/v1.1.0-rc4/spec.md#warnings
	//   - https://www.rfc-editor.org/rfc/rfc7234#section-5.5
	resp, err := r.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to upload blob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", handleRemoteError(resp)
	}

	blobLocation, err := resp.Location()
	if err != nil {
		output.Errorf("Warning: remote registry did not return blob location")
	}

	return blobLocation.String(), nil
}

// uploadBlobChunkedParallel performs a parallel chunked blob upload. It divides the blob
// into chunks and uploads them concurrently to maximize network throughput. This method
// requires the content source to be an io.ReaderAt for random access.
func (r *Repository) uploadBlobChunkedParallel(ctx context.Context, location *url.URL, postResp *http.Response, expected ocispec.Descriptor, content io.ReaderAt) (string, error) {
	numChunks := int(math.Ceil(float64(expected.Size) / float64(uploadChunkDefaultSize)))
	authHeader := postResp.Request.Header.Get("Authorization")

	g, gCtx := errgroup.WithContext(ctx)
	sem := semaphore.NewWeighted(uploadChunkConcurrency)

	for i := 0; i < numChunks; i++ {
		if err := sem.Acquire(gCtx, 1); err != nil {
			break // Context was cancelled
		}

		chunkIndex := i
		g.Go(func() error {
			defer sem.Release(1)

			start := int64(chunkIndex) * uploadChunkDefaultSize
			chunkLen := min(uploadChunkDefaultSize, expected.Size-start)
			output.SafeDebugf("Uploading chunk %d/%d, range %d-%d", chunkIndex+1, numChunks, start, start+chunkLen-1)

			// Read the specific chunk from the content source.
			chunkData := make([]byte, chunkLen)
			if _, err := content.ReadAt(chunkData, start); err != nil {
				return fmt.Errorf("chunk %d: failed to read content: %w", chunkIndex, err)
			}

			req, err := http.NewRequestWithContext(gCtx, http.MethodPatch, location.String(), bytes.NewReader(chunkData))
			if err != nil {
				return fmt.Errorf("chunk %d: failed to create request: %w", chunkIndex, err)
			}

			req.ContentLength = chunkLen
			req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", start, start+chunkLen-1))
			req.Header.Set("Content-Type", "application/octet-stream")
			if authHeader != "" {
				req.Header.Set("Authorization", authHeader)
			}

			resp, err := r.client().Do(req)
			if err != nil {
				return fmt.Errorf("chunk %d: failed to upload: %w", chunkIndex, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusAccepted {
				return handleRemoteError(resp)
			}
			// The location header in the response for a PATCH should be the same session URL.
			// We can ignore it in parallel uploads as we always use the initial session URL.
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return "", fmt.Errorf("failed during parallel chunk upload: %w", err)
	}

	// Final PUT request to mark upload as completed.
	return r.finalizeUpload(ctx, location, expected, authHeader)
}

// Renamed from uploadBlobChunked to uploadBlobChunkedSequential to clarify its behavior.
// This is the original sequential upload logic, now used as a fallback.
//
// uploadBlobChunkedSequential performs a chunked blob upload as per the distribution spec. The blob is divided into chunks of maximum 100MiB
// in size and uploaded sequentially through PATCH requests. Once entire blob is uploaded, a PUT request marks the upload as complete.
// Note that the distribution spec 1) requires blobs to uploaded in-order, and 2) does not have a way of specifying maximum blob
// size.
func (r *Repository) uploadBlobChunkedSequential(ctx context.Context, location *url.URL, postResp *http.Response, expected ocispec.Descriptor, content io.Reader) (string, error) {
	numChunks := int(math.Ceil(float64(expected.Size) / float64(uploadChunkDefaultSize)))
	authHeader := postResp.Request.Header.Get("Authorization")

	rangeStart := int64(0)
	rangeEnd := min(uploadChunkDefaultSize-1, expected.Size-1)
	nextLocation := location
	for i := 0; i < numChunks; i++ {
		output.SafeDebugf("Uploading chunk %d/%d, range %d-%d", i+1, numChunks, rangeStart, rangeEnd)

		bodyLength := rangeEnd - rangeStart + 1
		lr := io.LimitReader(content, int64(bodyLength))

		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, nextLocation.String(), lr)
		if err != nil {
			return "", err
		}
		req.ContentLength = bodyLength
		req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", rangeStart, rangeEnd))
		req.Header.Set("Content-Type", "application/octet-stream")
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}

		resp, err := r.client().Do(req)
		if err != nil {
			return "", fmt.Errorf("failed to upload blob chunk: %w", err)
		}
		if resp.StatusCode != http.StatusAccepted {
			defer resp.Body.Close()
			return "", handleRemoteError(resp)
		}
		resp.Body.Close()

		respLocation, err := resp.Location()
		if err != nil {
			return "", fmt.Errorf("missing Location header in response")
		}
		nextLocation = respLocation

		respRange := resp.Header.Get("Range")
		if respRange == "" {
			return "", fmt.Errorf("missing Range header in response")
		}
		startEnd := strings.Split(respRange, "-")
		if len(startEnd) != 2 || startEnd[0] != "0" {
			return "", fmt.Errorf("server returned invalid Range header: %s", respRange)
		}
		curEnd, err := strconv.ParseInt(startEnd[1], 10, 0)
		if err != nil {
			return "", fmt.Errorf("server returned invalid Range header: %s", respRange)
		}
		if curEnd != rangeEnd {
			return "", fmt.Errorf("mismatch in range header: expected 0-%d, actual 0-%d", rangeEnd, curEnd)
		}

		rangeStart = rangeEnd + 1
		rangeEnd = min(expected.Size-1, rangeEnd+uploadChunkDefaultSize)
	}

	return r.finalizeUpload(ctx, nextLocation, expected, authHeader)
}

// finalizeUpload sends the final PUT request to the registry to complete a chunked upload.
func (r *Repository) finalizeUpload(ctx context.Context, location *url.URL, expected ocispec.Descriptor, authHeader string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, location.String(), nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	q.Set("digest", expected.Digest.String())
	req.URL.RawQuery = q.Encode()
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	output.SafeDebugf("Finalizing upload")
	resp, err := r.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to finalize blob upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", handleRemoteError(resp)
	}

	blobLocation, err := resp.Location()
	if err != nil {
		output.Errorf("Warning: remote registry did not return blob location")
	}

	return blobLocation.String(), nil
}

// client returns an HTTP client used to access the remote repository.
// A default HTTP client is return if the client is not configured.
func (r *Repository) client() remote.Client {
	if r.Client == nil {
		return auth.DefaultClient
	}
	return r.Client
}

func buildRepositoryBlobUploadURL(plainHTTP bool, ref registry.Reference) string {
	scheme := "https"
	if plainHTTP {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/v2/%s/blobs/uploads/", scheme, ref.Host(), ref.Repository)
}

// defaultUploadConcurrency determines a reasonable default for parallel upload
// operations. It scales with the number of CPUs but is capped to avoid spawning
// excessive goroutines. Users can override this value by setting the
// KITOPS_UPLOAD_CONCURRENCY environment variable.
func defaultUploadConcurrency() int64 {
	if val := os.Getenv("KITOPS_UPLOAD_CONCURRENCY"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			return int64(n)
		}
	}

	cpus := runtime.NumCPU()
	conc := int64(cpus * 4)
	if conc < 4 {
		conc = 4
	}
	if conc > 64 {
		conc = 64
	}
	return conc
}
