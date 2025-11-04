// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCPPScan(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name         string
		buf          string
		wantIncludes []string
		wantDefines  map[string][]string
	}{
		{
			name: "helloworld",
			buf: `
#include <stdio.h>

int main(int arg, char *argv[]) {
  printf("hello, world\n");
}
`,
			wantIncludes: []string{
				"<stdio.h>",
			},
			wantDefines: map[string][]string{},
		},
		{
			name: "base",
			buf: `
// Copyright 2012 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

#ifndef BASE_VERSION_H_
#define BASE_VERSION_H_

#include <stdint.h>

#include <iosfwd>
#include <string>
#include <vector>

#include "base/base_export.h"
#include "base/strings/string_piece.h"

namespace base {
 ...
}
`,
			wantIncludes: []string{
				"<stdint.h>",
				"<iosfwd>",
				"<string>",
				"<vector>",
				`"base/base_export.h"`,
				`"base/strings/string_piece.h"`,
			},
			wantDefines: map[string][]string{},
		},
		{
			name: "macros",
			buf: `
#ifndef CONFIG_H
#define CONFIG_H

#ifdef NDEBUG
 # define USER_CONFIG_H "user_release.h"
#else
 # define USER_CONFIG_H "user_debug.h"
#endif

#include USER_CONFIG_H

#endif // CONFIG_H
`,
			wantIncludes: []string{
				"USER_CONFIG_H",
			},
			wantDefines: map[string][]string{
				"USER_CONFIG_H": {`"user_release.h"`, `"user_debug.h"`},
			},
		},
		{
			name: "unsupported",
			buf: `
#include /* comment */ "foo.h"
#include /* comment */ <bar.h>
#include \
 "baz.h"
# \
 include "quux.h"

#define FOO_H /* comment */ BAR_H
#define BAR_H \
 BAZ("baz")
#define BAZ(x) x # ".h"
`,
			wantDefines: map[string][]string{},
		},
		{
			name: "objc-import",
			buf: `
#import "HTTPRequest.h"
`,
			wantIncludes: []string{
				`"HTTPRequest.h"`,
			},
			wantDefines: map[string][]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotIncludes, gotDefines, err := CPPScan(ctx, tc.name, []byte(tc.buf))
			if err != nil {
				t.Errorf("CPPScan(ctx,%q,buf)=%v,%v,%v; want nil error", tc.name, gotIncludes, gotDefines, err)
			}
			if diff := cmp.Diff(tc.wantIncludes, gotIncludes); diff != "" {
				t.Errorf("CPPScan(ctx,%q,buf) includes diff -want +got:\n%s", tc.name, diff)
			}
			if diff := cmp.Diff(tc.wantDefines, gotDefines); diff != "" {
				t.Errorf("CPPScan(ctx,%q,buf) defines diff -want +got:\n%s", tc.name, diff)
			}
		})
	}
}

func TestAddInclude(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "bracket",
			input: "<foo.h>",
			want:  []string{"<foo.h>"},
		},
		{
			name:  "bracket-extra",
			input: "<foo.h> // comment",
			want:  []string{"<foo.h>"},
		},
		{
			name:  "quote",
			input: `"foo.h"`,
			want:  []string{`"foo.h"`},
		},
		{
			name:  "quote-extra",
			input: `"foo.h"  // comment`,
			want:  []string{`"foo.h"`},
		},
		{
			name:  "macro",
			input: "FOO_H",
			want:  []string{"FOO_H"},
		},
		{
			name:  "macro-extra",
			input: "FOO_H // comment",
			want:  []string{"FOO_H"},
		},
		{
			name:  "number",
			input: "1",
		},
		{
			name:  "lower-macro",
			input: "foo_h",
		},
		{
			name:  "comment",
			input: `/* comment */ "foo.h"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := addInclude(ctx, nil, []byte(tc.input))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("addInclude(ctx, nil, %q): diff -want +got:\n%s", tc.input, diff)
			}
		})
	}
}

func TestAddDefine(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		input string
		want  map[string][]string
	}{
		{
			name:  "string-literal",
			input: `FOO "foo.h"`,
			want: map[string][]string{
				"FOO": {`"foo.h"`},
			},
		},
		{
			name:  "bracket",
			input: "FOO <foo.h>",
			want: map[string][]string{
				"FOO": {"<foo.h>"},
			},
		},
		{
			name:  "OTHER_MACRO",
			input: "FOO BAR_H",
			want: map[string][]string{
				"FOO": {"BAR_H"},
			},
		},
		{
			name:  "other-value",
			input: "FOO foo",
			want:  map[string][]string{},
		},
		{
			name:  "number",
			input: "FOO 1",
			want:  map[string][]string{},
		},
		{
			name:  "lower-macro",
			input: "FOO macro_value",
			want:  map[string][]string{},
		},
		{
			name:  "func-macro",
			input: "FOO(x) BAR(x)",
			want:  map[string][]string{},
		},
		{
			name:  "comment",
			input: "FOO /* comment */ BAR",
			want:  map[string][]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defines := make(map[string][]string)
			addDefine(ctx, defines, []byte(tc.input))
			if diff := cmp.Diff(tc.want, defines); diff != "" {
				t.Errorf("addDefines(ctx,defines,file,%q): diff -want +got:\n%s", tc.input, diff)
			}
		})
	}
}

func TestExpandMacros(t *testing.T) {
	ctx := t.Context()

	for _, tc := range []struct {
		name    string
		incname string
		macros  map[string][]string
		want    []string
	}{
		{
			name: "empty",
		},
		{
			name:    "string-literal",
			incname: `"foo.h"`,
			want:    []string{`"foo.h"`},
		},
		{
			name:    "bracket",
			incname: "<foo.h>",
			want:    []string{"<foo.h>"},
		},
		{
			name:    "expand",
			incname: "FOO_H",
			macros: map[string][]string{
				"FOO_H": {`"foo.h"`},
			},
			want: []string{`"foo.h"`},
		},
		{
			name:    "expand-multi",
			incname: "FOO_H",
			macros: map[string][]string{
				"FOO_H": {`"foo.h"`, `"foo_debug.h"`},
			},
			want: []string{`"foo.h"`, `"foo_debug.h"`},
		},
		{
			name:    "expand-rec",
			incname: "FOO_H",
			macros: map[string][]string{
				"FOO_H": {"BAR_H"},
				"BAR_H": {`"bar.h"`},
			},
			want: []string{`"bar.h"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			got = cppExpandMacros(ctx, got, tc.incname, tc.macros)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("expandMacros(ctx, %q, macros): diff -want +got:\n%s", tc.incname, diff)
			}
		})
	}
}
