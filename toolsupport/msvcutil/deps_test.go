// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package msvcutil_test

import (
	"bytes"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/toolsupport/msvcutil"
)

func TestParseShowIncludes(t *testing.T) {
	inputs, out := msvcutil.ParseShowIncludes([]byte("Note: including file: ../../base/features.h\r\n" +
		"Note: including file:  ../../base/base_export.h\r\n" +
		"Note: including file:  ../../base/feature_list.h\r\n" +
		"Note: including file:   ../../buildtools/third_party/libc++/trunk/include\\atomic\r\n" +
		"In file included from ../../base/version.cc\r\n" +
		"In file included from ../../base/feature_list.h\r\n" +
		"fatal error: 'functional' not found\r\n" +
		"#include <functional>\r\n" +
		"         ^~~~~~~~~~~~\r\n"))
	wantInputs := []string{
		"../../base/features.h",
		"../../base/base_export.h",
		"../../base/feature_list.h",
		`../../buildtools/third_party/libc++/trunk/include\atomic`,
	}
	if diff := cmp.Diff(wantInputs, inputs); diff != "" {
		t.Errorf("msvcutil.ParseShowIncludes(...); inputs diff -want +got:\n%s", diff)
	}
	wantOut := []byte(`In file included from ../../base/version.cc
In file included from ../../base/feature_list.h
fatal error: 'functional' not found
#include <functional>
         ^~~~~~~~~~~~
`)
	wantOut = bytes.ReplaceAll(wantOut, []byte("\n"), []byte("\r\n"))
	if !bytes.Equal(wantOut, out) {
		t.Errorf("msvcutil.ParseShowIncludes(...) got:\n%q\nwant:\n%q", out, wantOut)
	}
}

func TestParseShowIncludes_filename(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stdout     []byte
		wantInputs []string
		wantOut    []byte
	}{
		{
			name:    "filterFilename",
			stdout:  []byte("foo.cc\r\ncl: warning\r\n"),
			wantOut: []byte("cl: warning\r\n"),
		},
		{
			name:       "keepFilenameAfterShowIncludes",
			stdout:     []byte("foo.cc\r\nNote: including file: foo.h\r\nsomething something foo.cc\r\n"),
			wantInputs: []string{"foo.h"},
			wantOut:    []byte("something something foo.cc\r\n"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs, out := msvcutil.ParseShowIncludes(tc.stdout)
			if diff := cmp.Diff(tc.wantInputs, inputs); diff != "" {
				t.Errorf("msvcutil.ParseShowIncludes(...); inputs diff -want +got:\n%s", diff)
			}
			if !bytes.Equal(tc.wantOut, out) {
				t.Errorf("msvcutil.ParseShowIncludes(...) got:\n%q\nwant:\n%q", out, tc.wantOut)
			}
		})
	}
}

func TestDepsArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "hasShowIncludes",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang-cl",
				"/showIncludes",
				"/c",
				"../../base/version.cc",
				"/Foobj/base/version.obj",
			},
			want: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang-cl",
				"/showIncludes",
				"/P",
				"../../base/version.cc",
			},
		},
		{
			name: "hasShowIncludesUser",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang-cl",
				"/showIncludes:user",
				"/c",
				"../../base/version.cc",
				"/Foobj/base/version.obj",
			},
			want: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang-cl",
				"/showIncludes",
				"/P",
				"../../base/version.cc",
			},
		},
		{
			name: "noShowIncludes",
			args: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang-cl",
				"/c",
				"../../base/version.cc",
				"/Foobj/base/version.obj",
			},
			want: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang-cl",
				"/P",
				"../../base/version.cc",
				"/showIncludes",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := msvcutil.DepsArgs(tc.args)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("msvcutil.DepsArgs(%q): diff (-want +got):\n%s", tc.args, diff)
			}
		})
	}
}
