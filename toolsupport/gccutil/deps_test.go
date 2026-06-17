// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package gccutil_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/toolsupport/gccutil"
)

func TestDepsArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "separateFlag",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"-MMD",
				"-MF",
				"obj/base/version.o.d",
				"-c",
				"../../base/version.cc",
				"-o",
				"obj/base/version.o",
			},
			want: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"../../base/version.cc",
				"-M",
			},
		},
		{
			name: "joinedFlag",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"-MMD",
				"-MFobj/base/version.o.d",
				"-c",
				"../../base/version.cc",
				"-oobj/base/version.o",
			},
			want: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"../../base/version.cc",
				"-M",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gccutil.DepsArgs(tc.args)
			if err != nil {
				t.Fatalf("gccutil.DepsArgs(%q)=%v, %v; want nil err", tc.args, got, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("gccutil.DepsArgs(%q): diff (-want +got):\n%s", tc.args, diff)
			}
		})
	}
}
