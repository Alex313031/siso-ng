// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// firedTimeoutCtx returns a WithTimeoutCause context whose deadline has
// already elapsed, so context.Cause(ctx) == cause. Models our watchdog
// having fired by the time keepFirstAttempt runs.
func firedTimeoutCtx(t *testing.T, cause error) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeoutCause(t.Context(), time.Nanosecond, cause)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

// TestKeepFirstAttempt pins the keep/fall-back decision against the
// deadline race: a completed attempt (cache hit or final error) must be
// kept; only one our deadline actually cut off may fall back.
func TestKeepFirstAttempt(t *testing.T) {
	cause := status.Error(codes.Aborted, "GetActionResult first-byte timeout")
	deadlineErr := status.Error(codes.DeadlineExceeded, "context deadline exceeded")

	// liveCtx never reaches its deadline: context.Cause(ctx) == nil.
	liveCtx, cancelLive := context.WithTimeoutCause(t.Context(), time.Hour, cause)
	t.Cleanup(cancelLive)

	// parentExpiredCtx: the parent's own deadline expired, not ours, so
	// the cause is the parent's, not ours, even after the child fires.
	parentExpired := func() context.Context {
		parent, cancelParent := context.WithTimeoutCause(t.Context(), time.Nanosecond, errors.New("parent deadline"))
		t.Cleanup(cancelParent)
		<-parent.Done()
		child, cancelChild := context.WithTimeoutCause(parent, time.Hour, cause)
		t.Cleanup(cancelChild)
		return child
	}()

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		callErr error
		want    bool // keep the first attempt's result?
	}{
		{
			// The watchdog genuinely interrupted the in-flight call.
			name:    "our deadline cut the call off",
			ctx:     firedTimeoutCtx(t, cause),
			callErr: deadlineErr,
			want:    false, // fall back to a fresh attempt
		},
		{
			// The cache hit returned just as the deadline fired. The
			// regression: this result must not be discarded.
			name:    "cache hit raced the deadline",
			ctx:     firedTimeoutCtx(t, cause),
			callErr: nil,
			want:    true,
		},
		{
			// A definitive NotFound returned just as the deadline fired.
			// Keep it rather than redo the lookup (whose fallback could
			// itself fail and surface a worse error).
			name:    "final error raced the deadline",
			ctx:     firedTimeoutCtx(t, cause),
			callErr: status.Error(codes.NotFound, "no action result"),
			want:    true,
		},
		{
			name:    "deadline never fired",
			ctx:     liveCtx,
			callErr: nil,
			want:    true,
		},
		{
			// DeadlineExceeded, but from the parent's deadline, not ours.
			// Falling back under an already-expired parent is pointless.
			name:    "parent deadline, not ours",
			ctx:     parentExpired,
			callErr: deadlineErr,
			want:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepFirstAttempt(tc.ctx, cause, tc.callErr); got != tc.want {
				t.Errorf("keepFirstAttempt() = %v, want %v", got, tc.want)
			}
		})
	}
}
