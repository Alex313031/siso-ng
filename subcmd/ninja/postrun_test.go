// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/subcommands"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/reapi"
)

func TestPostRun_ResultInfraFailure(t *testing.T) {
	tests := []struct {
		name             string
		err              error
		wantInfraFailure bool
	}{
		{
			name: "exit_error",
			err: ninjabuild.BuildError{
				Err: build.TooManyFallbackError{
					Err: execute.ExitError{ExitCode: 1},
				},
			},
			wantInfraFailure: false,
		},
		{
			name: "deps_error",
			err: ninjabuild.BuildError{
				Err: build.TooManyFallbackError{
					Err: build.DepsError{Err: errors.New("deps inputs have no dependencies")},
				},
			},
			wantInfraFailure: false,
		},
		{
			name: "deps_error_unsandboxed",
			err: ninjabuild.BuildError{
				Err: build.TooManyFallbackError{
					Err: build.DepsError{UnsandboxedInputs: []string{"../../undeclared.h"}},
				},
			},
			wantInfraFailure: false,
		},
		{
			name: "infra_failure",
			err: ninjabuild.BuildError{
				Err: build.TooManyFallbackError{
					Err: errors.New("remote exec worker unavailable"),
				},
			},
			wantInfraFailure: true,
		},
		{
			name:             "auth_error",
			err:              fmt.Errorf("failed to initialize credentials: %w", cred.AuthError{Err: errors.New("need to run `siso login`")}),
			wantInfraFailure: true,
		},
		{
			name:             "unauthenticated",
			err:              fmt.Errorf("failed to get capabilities: %w", status.Error(codes.Unauthenticated, "invalid authentication credentials")),
			wantInfraFailure: true,
		},
		{
			name:             "permission_denied",
			err:              fmt.Errorf("failed to check writable: %w", status.Error(codes.PermissionDenied, "caller does not have permission")),
			wantInfraFailure: true,
		},
		{
			name: "build_auth_error",
			err: ninjabuild.BuildError{
				Err: build.StepError{
					Cause: status.Error(codes.Unauthenticated, "invalid authentication credentials"),
				},
			},
			wantInfraFailure: true,
		},
		{
			name: "bad_platform_container_image",
			err: ninjabuild.BuildError{
				Err: build.StepError{
					Cause: fmt.Errorf("%w: %w", reapi.ErrBadPlatformContainerImage, status.Error(codes.PermissionDenied, "container image not found")),
				},
			},
			wantInfraFailure: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logDir := t.TempDir()
			c := &Command{
				started: time.Now(),
			}
			c.logDir = logDir
			status := c.postRun(t.Context(), build.Stats{}, tc.err)
			if status != subcommands.ExitFailure {
				t.Errorf("c.postRun = %v, want ExitFailure", status)
			}
			data, err := os.ReadFile(filepath.Join(logDir, sisoResultFilename))
			if err != nil {
				t.Fatalf("failed to read siso_result.json: %v", err)
			}
			var res SisoResult
			if err := json.Unmarshal(data, &res); err != nil {
				t.Fatalf("failed to unmarshal siso_result.json: %v", err)
			}
			if res.InfraFailure != tc.wantInfraFailure {
				t.Errorf("res.InfraFailure = %t, want %t (res: %+v)", res.InfraFailure, tc.wantInfraFailure, res)
			}
		})
	}
}
