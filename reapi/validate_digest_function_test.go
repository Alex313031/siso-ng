// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"testing"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"
)

// capWith builds ServerCapabilities advertising the given digest functions.
func capWith(fns ...rpb.DigestFunction_Value) *rpb.ServerCapabilities {
	return &rpb.ServerCapabilities{
		CacheCapabilities: &rpb.CacheCapabilities{
			DigestFunctions: fns,
		},
	}
}

func TestValidateDigestFunction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current rpb.DigestFunction_Value
		capa    *rpb.ServerCapabilities
		wantErr bool
	}{
		{
			name:    "empty_list_sha256_inferred_ok",
			current: rpb.DigestFunction_SHA256,
			capa:    capWith(),
			wantErr: false,
		},
		{
			name:    "empty_list_blake3_rejected",
			current: rpb.DigestFunction_BLAKE3,
			capa:    capWith(),
			wantErr: true,
		},
		{
			name:    "missing_sha256_rejected",
			current: rpb.DigestFunction_SHA256,
			capa:    capWith(rpb.DigestFunction_BLAKE3),
			wantErr: true,
		},
		{
			name:    "blake3_supported",
			current: rpb.DigestFunction_BLAKE3,
			capa:    capWith(rpb.DigestFunction_SHA256, rpb.DigestFunction_BLAKE3),
			wantErr: false,
		},
		{
			name:    "sha256_supported",
			current: rpb.DigestFunction_SHA256,
			capa:    capWith(rpb.DigestFunction_SHA256, rpb.DigestFunction_BLAKE3),
			wantErr: false,
		},
		{
			name:    "exec_missing_blake3_rejected",
			current: rpb.DigestFunction_BLAKE3,
			capa: &rpb.ServerCapabilities{
				CacheCapabilities: &rpb.CacheCapabilities{
					DigestFunctions: []rpb.DigestFunction_Value{rpb.DigestFunction_SHA256, rpb.DigestFunction_BLAKE3},
				},
				ExecutionCapabilities: &rpb.ExecutionCapabilities{
					ExecEnabled:     true,
					DigestFunctions: []rpb.DigestFunction_Value{rpb.DigestFunction_SHA256},
				},
			},
			wantErr: true,
		},
		{
			name:    "exec_blake3_supported",
			current: rpb.DigestFunction_BLAKE3,
			capa: &rpb.ServerCapabilities{
				CacheCapabilities: &rpb.CacheCapabilities{
					DigestFunctions: []rpb.DigestFunction_Value{rpb.DigestFunction_SHA256, rpb.DigestFunction_BLAKE3},
				},
				ExecutionCapabilities: &rpb.ExecutionCapabilities{
					ExecEnabled:     true,
					DigestFunctions: []rpb.DigestFunction_Value{rpb.DigestFunction_SHA256, rpb.DigestFunction_BLAKE3},
				},
			},
			wantErr: false,
		},
		{
			name:    "exec_legacy_singular_blake3_supported",
			current: rpb.DigestFunction_BLAKE3,
			capa: &rpb.ServerCapabilities{
				CacheCapabilities: &rpb.CacheCapabilities{
					DigestFunctions: []rpb.DigestFunction_Value{rpb.DigestFunction_SHA256, rpb.DigestFunction_BLAKE3},
				},
				ExecutionCapabilities: &rpb.ExecutionCapabilities{
					ExecEnabled:    true,
					DigestFunction: rpb.DigestFunction_BLAKE3,
				},
			},
			wantErr: false,
		},
		{
			name:    "exec_no_digest_functions_rejected",
			current: rpb.DigestFunction_BLAKE3,
			capa: &rpb.ServerCapabilities{
				CacheCapabilities: &rpb.CacheCapabilities{
					DigestFunctions: []rpb.DigestFunction_Value{rpb.DigestFunction_SHA256, rpb.DigestFunction_BLAKE3},
				},
				ExecutionCapabilities: &rpb.ExecutionCapabilities{
					ExecEnabled: true,
				},
			},
			wantErr: true,
		},
		{
			name:    "exec_disabled_cache_blake3_supported",
			current: rpb.DigestFunction_BLAKE3,
			capa: &rpb.ServerCapabilities{
				CacheCapabilities: &rpb.CacheCapabilities{
					DigestFunctions: []rpb.DigestFunction_Value{rpb.DigestFunction_SHA256, rpb.DigestFunction_BLAKE3},
				},
				ExecutionCapabilities: &rpb.ExecutionCapabilities{
					ExecEnabled: false,
				},
			},
			wantErr: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn, err := digest.Lookup(tc.current)
			if err != nil {
				t.Fatalf("digest.Lookup(%v): %v", tc.current, err)
			}

			err = validateDigestFunction(fn, tc.capa)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("validateDigestFunction(%v) error = %v, wantErr = %v", tc.current, err, tc.wantErr)
			}
		})
	}
}
