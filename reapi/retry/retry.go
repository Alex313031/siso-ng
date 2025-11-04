// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package retry provides retrying functionalities.
package retry

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/o11y/clog"
)

// ExponentialBackoff handles exponential backoff.
type ExponentialBackoff struct {
	started time.Time
	retries int
	delay   time.Duration

	// retried for auth failure.
	// we allow auth retry at most once, since next call should
	// succeed with credential refresh.
	authRetry bool
}

func (b *ExponentialBackoff) retriableError(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		st = status.FromContextError(err)
	}

	// https://github.com/bazelbuild/bazel/blob/7.1.1/src/main/java/com/google/devtools/build/lib/remote/RemoteRetrier.java#L47
	switch st.Code() {
	case codes.ResourceExhausted,
		codes.Internal,
		codes.Unavailable,
		codes.Aborted:
		return true
	case codes.Unknown:
		// unknown grpc error should retry, but non grpc error should not.
		return ok
	case
		// may get
		// code = Unauthenticated desc = Request had invalid authentication credentials.
		// Expected OAuth 2 access token, login cookie or other valid authentication credential.
		// See https://developers.google.com/identity/sign-in/web/devconsole-project.
		// (access token expired, need to refresh).
		// or
		// code = PermissionDenied desc = The caller does not have permission
		//
		// but should not retry (wrong auth, instance without permission)
		// code = PermissionDenied desc = Permission "xx" denied on resource "yy" (or it may not exist)
		// code = PermissionDenied desc = Permission denied on resource project xx
		// code = PermissionDenied desc = Remote Build Execution API has not been used in project rbe-android before or it is disabled. Enable it by visiting https://console.developers.google.com/apis/api/remotebuildexecution.googleapis.com/overview?project=rbe-android then retry. If you enabled this API recently, wait a few minutes for the action to propagate to our systems and retry.
		codes.Unauthenticated,
		codes.PermissionDenied:
		// Allow authRetry at most once.
		// Next call should not fail with auth error if it was
		// token expired.
		// It does not make sense to retry more if it is lack of
		// permission or so.
		retry := b.authRetry
		b.authRetry = true
		return !retry
	}
	return false
}

// Next returns next backoff delay.
// If delay is 0, no need to retry any more.
func (b *ExponentialBackoff) Next(ctx context.Context, err error) (time.Duration, error) {
	if err == nil {
		return 0, nil
	}
	const maxRetries = 10
	const multiplier = 2
	const baseDelay = 200 * time.Millisecond
	const maxDelay = float64(10 * time.Second)
	const backoffRange = 0.4
	if b.started.IsZero() {
		b.started = time.Now()
	}
	if b.delay == 0 {
		b.delay = baseDelay
	}

	if !b.retriableError(err) {
		return 0, err
	}

	if b.retries >= maxRetries {
		return 0, fmt.Errorf("too many retries %d %s: %w", b.retries, time.Since(b.started), err)
	}
	b.retries++
	backoff := float64(b.delay) * multiplier
	if backoff > maxDelay {
		backoff = maxDelay
	}
	backoff -= backoff * backoffRange * rand.Float64()
	b.delay = time.Duration(backoff)
	if b.delay < baseDelay {
		b.delay = baseDelay
	}
	return b.delay, err
}

// Do calls function `f` and retries with exponential backoff for errors that are known to be retriable.
func Do(ctx context.Context, f func() error) error {
	var backoff ExponentialBackoff
	for {
		err := f()
		delay, err := backoff.Next(ctx, err)
		if delay == 0 {
			return err
		}
		clog.Warningf(ctx, "retry backoff=%s: %v", delay, err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}
