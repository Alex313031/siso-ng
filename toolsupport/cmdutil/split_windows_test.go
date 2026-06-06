// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package cmdutil

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmdline string
		want    []string
	}{
		{
			name:    "simple",
			cmdline: `..\..\third_party\llvm-build\Release+Asserts\bin\clang-cl.exe /c foo.cc`,
			want: []string{
				`..\..\third_party\llvm-build\Release+Asserts\bin\clang-cl.exe`,
				"/c",
				"foo.cc",
			},
		},
		{
			name:    "long",
			cmdline: `python3.exe copy-files.py src dst files` + strings.Repeat("a,", 8192/2),
			want: []string{
				"python3.exe",
				"copy-files.py",
				"src",
				"dst",
				"files" + strings.Repeat("a,", 8192/2),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Split(tc.cmdline)
			if err != nil {
				t.Fatalf("Split(%q)=%q, %v; want nil err", tc.cmdline, got, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Split(%q) -want +got:\n%s", tc.cmdline, diff)
			}
		})
	}
}
