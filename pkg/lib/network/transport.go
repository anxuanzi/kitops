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

package network

import (
	"crypto/tls"
	"net"
	"net/http"
	"runtime"
	"time"
)

// OptimizedTransport creates a Docker CLI-inspired HTTP transport with optimizations
// for container registry operations
func OptimizedTransport(tlsConfig *tls.Config) *http.Transport {
	// Base transport with Docker CLI-inspired settings
	transport := &http.Transport{
		// Connection pooling optimizations
		MaxIdleConns:        100,              // Docker CLI uses 100
		MaxIdleConnsPerHost: 10,               // Docker CLI uses 10
		MaxConnsPerHost:     0,                // No limit, let connection pooling handle it
		IdleConnTimeout:     90 * time.Second, // Docker CLI uses 90s

		// TCP optimizations
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second, // Connection timeout
			KeepAlive: 30 * time.Second, // TCP keep-alive
			DualStack: true,             // Enable IPv4 and IPv6
		}).DialContext,

		// TLS optimizations
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: 10 * time.Second, // TLS handshake timeout

		// Response handling optimizations
		ResponseHeaderTimeout: 30 * time.Second, // Response header timeout
		ExpectContinueTimeout: 1 * time.Second,  // Expect: 100-continue timeout

		// Compression handling - Docker CLI enables this
		DisableCompression: false, // Enable gzip compression

		// HTTP/2 will be configured separately
		ForceAttemptHTTP2: true, // Force HTTP/2 attempt
	}

	// Apply adaptive connection limits based on system resources
	maxIdle, maxIdlePerHost, maxPerHost := AdaptiveConnectionLimits()
	transport.MaxIdleConns = maxIdle
	transport.MaxIdleConnsPerHost = maxIdlePerHost
	transport.MaxConnsPerHost = maxPerHost

	return transport
}

// GetOptimizedDialer returns a network dialer optimized for registry operations
func GetOptimizedDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}
}

// GetOptimizedHTTPClient returns an HTTP client optimized for registry operations
func GetOptimizedHTTPClient(tlsConfig *tls.Config) *http.Client {
	transport := OptimizedTransport(tlsConfig)

	return &http.Client{
		Transport: transport,
		// Timeout for the entire request (Docker CLI uses no timeout, but we set a reasonable one)
		Timeout: 5 * time.Minute,
		// Don't follow redirects automatically for registry operations
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// AdaptiveConnectionLimits adjusts connection limits based on system resources
func AdaptiveConnectionLimits() (maxIdleConns, maxIdleConnsPerHost, maxConnsPerHost int) {
	cpus := runtime.NumCPU()

	// Base limits similar to Docker CLI
	maxIdleConns = 100
	maxIdleConnsPerHost = 10
	maxConnsPerHost = 0 // Unlimited

	// Scale based on CPU count for better performance on multi-core systems
	if cpus > 4 {
		maxIdleConns = 100 + (cpus-4)*10      // Scale up idle connections
		maxIdleConnsPerHost = 10 + (cpus-4)*2 // Scale up per-host connections
	}

	// Cap at reasonable limits to avoid resource exhaustion
	if maxIdleConns > 200 {
		maxIdleConns = 200
	}
	if maxIdleConnsPerHost > 20 {
		maxIdleConnsPerHost = 20
	}

	return maxIdleConns, maxIdleConnsPerHost, maxConnsPerHost
}
