// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"
	semverpb "go.chromium.org/build/remote-apis/build/bazel/semver"
)

func capaWithVersions(low, high Version) *rpb.ServerCapabilities {
	return &rpb.ServerCapabilities{
		LowApiVersion:  &semverpb.SemVer{Major: low.Major, Minor: low.Minor},
		HighApiVersion: &semverpb.SemVer{Major: high.Major, Minor: high.Minor},
	}
}

func TestNegotiateAPIVersion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		capa      *rpb.ServerCapabilities
		requested string
		want      Version
		wantErr   bool
	}{
		{
			name: "server_2.0_only",
			capa: capaWithVersions(Version{2, 0}, Version{2, 0}),
			want: Version{2, 0},
		},
		{
			name: "server_unset_is_2.0",
			capa: &rpb.ServerCapabilities{},
			want: Version{2, 0},
		},
		{
			name: "server_high_below_siso_max",
			capa: capaWithVersions(Version{2, 0}, Version{2, 2}),
			want: Version{2, 2},
		},
		{
			name: "server_high_capped_at_siso_max",
			capa: capaWithVersions(Version{2, 0}, Version{2, 12}),
			want: maxAPIVersion,
		},
		{
			name: "server_low_at_siso_max",
			capa: capaWithVersions(Version{2, 3}, Version{2, 12}),
			want: maxAPIVersion,
		},
		{
			name:    "server_low_above_siso_max",
			capa:    capaWithVersions(Version{2, 4}, Version{2, 12}),
			wantErr: true,
		},
		{
			name:    "server_3_only",
			capa:    capaWithVersions(Version{3, 0}, Version{3, 1}),
			wantErr: true,
		},
		{
			name:      "requested_in_range",
			capa:      capaWithVersions(Version{2, 0}, Version{2, 12}),
			requested: "v2.1",
			want:      Version{2, 1},
		},
		{
			name:      "requested_above_server_high",
			capa:      capaWithVersions(Version{2, 0}, Version{2, 0}),
			requested: "v2.1",
			wantErr:   true,
		},
		{
			name:      "requested_below_server_low",
			capa:      capaWithVersions(Version{2, 1}, Version{2, 3}),
			requested: "v2.0",
			wantErr:   true,
		},
		{
			name:      "requested_above_siso_max",
			capa:      capaWithVersions(Version{2, 0}, Version{2, 12}),
			requested: "v2.4",
			wantErr:   true,
		},
		{
			name:      "requested_malformed",
			capa:      capaWithVersions(Version{2, 0}, Version{2, 0}),
			requested: "2.0",
			wantErr:   true,
		},
		{
			name:      "requested_trailing_garbage",
			capa:      capaWithVersions(Version{2, 0}, Version{2, 0}),
			requested: "v2.0x",
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := negotiateAPIVersion(tc.capa, tc.requested)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("negotiateAPIVersion(%v, %q) = %v, %v; wantErr=%t", tc.capa, tc.requested, got, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got != tc.want {
				t.Errorf("negotiateAPIVersion(%v, %q) = %s; want %s", tc.capa, tc.requested, got, tc.want)
			}
		})
	}
}

func TestAPIVersionFeatures(t *testing.T) {
	for _, tc := range []struct {
		ver                Version
		wantOutputPaths    bool
		wantActionPlatform bool
	}{
		{ver: Version{}},
		{ver: Version{2, 0}},
		{ver: Version{2, 1}, wantOutputPaths: true},
		{ver: Version{2, 2}, wantOutputPaths: true, wantActionPlatform: true},
		{ver: Version{2, 3}, wantOutputPaths: true, wantActionPlatform: true},
		{ver: Version{3, 0}, wantOutputPaths: true, wantActionPlatform: true},
	} {
		if got := tc.ver.UseOutputPaths(); got != tc.wantOutputPaths {
			t.Errorf("%v.UseOutputPaths() = %t; want %t", tc.ver, got, tc.wantOutputPaths)
		}
		if got := tc.ver.useOutputSymlinks(); got != tc.wantOutputPaths {
			t.Errorf("%v.useOutputSymlinks() = %t; want %t", tc.ver, got, tc.wantOutputPaths)
		}
		if got := tc.ver.UseActionForPlatformProperties(); got != tc.wantActionPlatform {
			t.Errorf("%v.UseActionForPlatformProperties() = %t; want %t", tc.ver, got, tc.wantActionPlatform)
		}
	}
}
