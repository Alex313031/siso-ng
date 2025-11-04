// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package makeutil

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseDeps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		depsfile []byte
		want     []string
	}{
		{
			name:     "simple",
			depsfile: []byte("foo.o:\tbar baz qux"),
			want: []string{
				"bar",
				"baz",
				"qux",
			},
		},
		{
			name:     "spaceinname",
			depsfile: []byte(`foo\ bar.o: baz\ qux`),
			want: []string{
				"baz qux",
			},
		},
		{
			name:     "newlinewhitespaces",
			depsfile: []byte("foo.o :\tbar\\\n\tbaz\\\r\n  qux"),
			want: []string{
				"bar",
				"baz",
				"qux",
			},
		},
		{
			name:     "backslashes",
			depsfile: []byte("foo\\bar.o: baz\\qux\\\n  quux\\corge"),
			want: []string{
				`baz\qux`,
				`quux\corge`,
			},
		},
		{
			name: "rust-multi",
			depsfile: []byte(`clang_x64_for_rust_host_build_tools/obj/third_party/rust/unicode_ident/v1/lib/libunicode_ident-unicode_ident-1.rlib: ../../third_party/rust/unicode_ident/v1/crate/src/lib.rs ../../third_party/rust/unicode_ident/v1/crate/src/tables.rs

../../third_party/rust/unicode_ident/v1/crate/src/lib.rs:
../../third_party/rust/unicode_ident/v1/crate/src/tables.rs:
`),
			want: []string{
				"../../third_party/rust/unicode_ident/v1/crate/src/lib.rs",
				"../../third_party/rust/unicode_ident/v1/crate/src/tables.rs",
			},
		},
		{
			name: "swift-multi",
			depsfile: []byte(`obj/ios/chrome/browser/shared/ui/util/util_swift/UIView+WindowCoordinates.o : ../../ios/chrome/browser/shared/ui/util/UIView+WindowCoordinates.swift ../../ios/chrome/browser/shared/ui/util/UIView+WindowObserving.swift
obj/ios/chrome/browser/shared/ui/util/util_swift/UIView+WindowObserving.o : ../../ios/chrome/browser/shared/ui/util/UIView+WindowCoordinates.swift ../../ios/chrome/browser/shared/ui/util/UIView+WindowObserving.swift
`),
			want: []string{
				"../../ios/chrome/browser/shared/ui/util/UIView+WindowCoordinates.swift",
				"../../ios/chrome/browser/shared/ui/util/UIView+WindowObserving.swift",
			},
		},
		{
			name:     "win-abs",
			depsfile: []byte(`foo\bar.o: c:\src\foo\bar.c`),
			want: []string{
				`c:\src\foo\bar.c`,
			},
		},
		{
			name: "tailing-backslash",
			depsfile: []byte(`gen/inputs.h: \
	/path/to/a.wsgl \
	/path/to/b.wsgl \
`),
			want: []string{
				"/path/to/a.wsgl",
				"/path/to/b.wsgl",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			got, err := ParseDeps(ctx, tc.depsfile)
			if err != nil {
				t.Errorf("ParseDeps(%q) got err=%v; want nil err", tc.depsfile, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ParseDeps(%q) -want +got:\n%s", tc.depsfile, diff)
			}
		})
	}
}

func TestParseDeps_Error(t *testing.T) {
	for _, tc := range []struct {
		name     string
		depsfile []byte
		want     []string
	}{
		{
			name:     "nooutput",
			depsfile: []byte(": bar baz qux"),
		},
		{
			name:     "multicolon",
			depsfile: []byte(`foo:bar:baz`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			got, err := ParseDeps(ctx, tc.depsfile)
			if err == nil {
				t.Errorf("ParseDeps(%q)=%q, %v; want err", tc.depsfile, got, err)
			}
		})
	}
}
