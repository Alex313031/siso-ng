// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package fetch provides the timeout policy for fetching blobs from the CAS.
package fetch

import (
	"context"
	"time"

	"go.chromium.org/build/hashigo/digest"
)

const slowThroughputPerSec = 1 * 1024 * 1024

// Timeout returns reasonable timeout to fetch d.
func Timeout(d digest.Digest) time.Duration {
	// 99p latency of BatchReadBlobs is 1.72s and ByteStream.Read is 0.522s as of 2025-08 in rbe-chromium-trusted
	return max(time.Duration(d.SizeBytes/slowThroughputPerSec)*time.Second, 10*time.Second)
}

// ContextWithTimeout returns context with timeout appropriate for d.
func ContextWithTimeout(ctx context.Context, d digest.Digest) (context.Context, context.CancelFunc) {
	timeout := max(time.Duration(d.SizeBytes/slowThroughputPerSec)*time.Second, 10*time.Minute)
	return context.WithTimeout(ctx, timeout)
}
