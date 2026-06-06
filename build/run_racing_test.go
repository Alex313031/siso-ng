// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/reapi/digest"
)

func TestIsContextCanceledErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "context.Canceled",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "wrapped context.Canceled",
			err:  fmt.Errorf("something: %w", context.Canceled),
			want: true,
		},
		{
			name: "grpc Canceled status",
			err:  status.Error(codes.Canceled, "context canceled"),
			want: true,
		},
		{
			name: "wrapped grpc Canceled status",
			err:  fmt.Errorf("find missing: %w", status.Error(codes.Canceled, "context canceled")),
			want: true,
		},
		{
			name: "deeply wrapped grpc Canceled (CAS upload path)",
			err: fmt.Errorf("failed to upload all foo: %w",
				fmt.Errorf("wait for digest=abc/123: %w",
					fmt.Errorf("find missing: %w",
						status.Error(codes.Canceled, "context canceled")))),
			want: true,
		},
		{
			name: "grpc DeadlineExceeded status",
			err:  status.Error(codes.DeadlineExceeded, "deadline exceeded"),
			want: false,
		},
		{
			name: "grpc Unavailable status",
			err:  status.Error(codes.Unavailable, "unavailable"),
			want: false,
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("something went wrong"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := isContextCanceledErr(tt.err), tt.want; got != want {
				t.Errorf("isContextCanceledErr(%v) = %v, want %v", tt.err, got, want)
			}
		})
	}
}

func TestRemoteClaimFallbackIfAllowed(t *testing.T) {
	tests := []struct {
		name               string
		err                error
		maxFallbackAllowed int64
		initialFallbacks   int64
		failuresAllowed    int
		cmdStdout          string
		wantOk             bool
		checkErr           func(*testing.T, error)
	}{
		{
			name:               "ContextCanceled",
			err:                context.Canceled,
			maxFallbackAllowed: 10,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("got %v, expected context.Canceled", err)
				}
			},
		},
		{
			name:               "DeadlineExceeded",
			err:                context.DeadlineExceeded,
			maxFallbackAllowed: 10,
			wantOk:             true,
			checkErr: func(t *testing.T, err error) {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			},
		},
		{
			name:               "ErrNotRelocatable",
			err:                errNotRelocatable,
			maxFallbackAllowed: 10,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, errNotRelocatable) {
					t.Errorf("got %v, expected errNotRelocatable", err)
				}
			},
		},
		{
			name:               "ErrNotInsideWorkspace",
			err:                errNotInsideWorkspace,
			maxFallbackAllowed: 10,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, errNotInsideWorkspace) {
					t.Errorf("got %v, expected errNotInsideWorkspace", err)
				}
			},
		},
		{
			name:               "ExitError_DefaultAllowFallback",
			err:                execute.ExitError{ExitCode: 1},
			maxFallbackAllowed: 10,
			wantOk:             true,
			checkErr: func(t *testing.T, err error) {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			},
		},
		{
			name:               "ExitError_PreferNoFallback",
			err:                execute.ExitError{ExitCode: 1},
			maxFallbackAllowed: 10,
			failuresAllowed:    1,
			cmdStdout:          "compile error",
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				var exitErr execute.ExitError
				if !errors.As(err, &exitErr) {
					t.Errorf("got %T: %v, expected execute.ExitError", err, err)
				} else if exitErr.ExitCode != 1 {
					t.Errorf("got exit code %d, expected 1", exitErr.ExitCode)
				}
			},
		},
		{
			name:               "ExitError_SIGKILL_AlwaysFallback",
			err:                execute.ExitError{ExitCode: 137},
			maxFallbackAllowed: 10,
			failuresAllowed:    1,
			cmdStdout:          "oom",
			wantOk:             true,
			checkErr: func(t *testing.T, err error) {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			},
		},
		{
			name:               "FallbackLimitExceeded",
			err:                status.Error(codes.Unavailable, "unavailable"),
			maxFallbackAllowed: 1,
			initialFallbacks:   1,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				var fallbackErr TooManyFallbackError
				wantErr := status.Error(codes.Unavailable, "unavailable")
				if !errors.As(err, &fallbackErr) {
					t.Errorf("got %T: %v, expected TooManyFallbackError", err, err)
				} else if !errors.Is(fallbackErr.Err, wantErr) {
					t.Errorf("got %v, expected wrapped error %v", fallbackErr.Err, wantErr)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			b := &Builder{
				maxFallbackAllowed: tt.maxFallbackAllowed,
			}
			b.numFallback.Store(tt.initialFallbacks)
			b.failures.allowed = tt.failuresAllowed
			if b.failures.allowed == 0 {
				b.failures.allowed = 2
			}

			cmd := &execute.Cmd{
				Args: []string{"clang++", "foo.cc"},
			}
			cmd.SetActionDigest(digest.Digest{Hash: "dummy", SizeBytes: 100})
			if tt.cmdStdout != "" {
				_, _ = cmd.StdoutWriter().Write([]byte(tt.cmdStdout))
			}

			step := &Step{
				outputPaths: []string{"foo.o"},
				cmd:         cmd,
				def:         fakeStepDef{},
			}

			ok, err := b.remoteClaimFallbackIfAllowed(ctx, step, tt.err)

			if ok != tt.wantOk {
				t.Errorf("remoteClaimFallbackIfAllowed() ok = %t; want %t", ok, tt.wantOk)
			}

			if tt.checkErr != nil {
				tt.checkErr(t, err)
			}
		})
	}
}
