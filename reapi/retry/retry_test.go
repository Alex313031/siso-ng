// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package retry_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/reapi/retry"
)

func TestDo_NoRetry(t *testing.T) {
	ctx := t.Context()
	called := 0
	err := retry.Do(ctx, func() error {
		called++
		return nil
	})
	if err != nil {
		t.Errorf("retry.Do=%v; want nil", err)
	}
	if called != 1 {
		t.Errorf("called=%d; want 1", called)
	}
}

func TestDo_NonRetriableError(t *testing.T) {
	ctx := t.Context()
	called := 0
	testErr := fmt.Errorf("error")
	err := retry.Do(ctx, func() error {
		called++
		return testErr
	})
	if !errors.Is(err, testErr) {
		t.Errorf("retry.Do=%v; want testErr", err)
	}
	if called != 1 {
		t.Errorf("called=%d; want 1", called)
	}
}

func TestDo_RetriableError(t *testing.T) {
	ctx := t.Context()
	called := 0
	err := retry.Do(ctx, func() error {
		called++
		if called == 1 {
			return status.Error(codes.Internal, "retriable error")
		}
		return nil
	})
	if err != nil {
		t.Errorf("retry.Do=%v; want nil", err)
	}
	if called != 2 {
		t.Errorf("called=%d; want 2", called)
	}
}

func TestDo_AuthError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 1*time.Second)
	defer cancel()
	called := 0
	err := retry.Do(ctx, func() error {
		called++
		return status.Error(codes.PermissionDenied, "permission denied")
	})
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("retry.Do=%v; want %v", err, codes.PermissionDenied)
	}
	if called != 2 {
		t.Errorf("called=%d; want 2", called)
	}
}
