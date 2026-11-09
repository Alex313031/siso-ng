// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"testing"
)

func TestInitOutputLocal_Minimum(t *testing.T) {
	ctx := t.Context()
	outputLocal, err := initOutputLocal(ctx, "minimum")
	if err != nil {
		t.Fatalf("initOutputLocal(%q): %v", "minimum", err)
	}
	for _, tc := range []struct {
		fname string
		want  bool
	}{
		{fname: "gen/foo.h", want: true},
		{fname: "gen/foo.hxx", want: true},
		{fname: "gen/foo.hpp", want: true},
		{fname: "gen/foo.inc", want: true},
		{fname: "gen/foo.c", want: true},
		{fname: "gen/foo.cc", want: true},
		{fname: "gen/foo.cxx", want: true},
		{fname: "gen/foo.cpp", want: true},
		{fname: "gen/foo.m", want: true},
		{fname: "gen/foo.mm", want: true},
		{fname: "gen/foo.json", want: true},
		{fname: "gen/foo.js", want: true},
		{fname: "gen/foo.ts", want: true},
		{fname: "gen/foo.py", want: true},
		{fname: "obj/foo/javac/foo.jar.jdeps", want: true},
		{fname: "obj/foo.o", want: false},
		{fname: "obj/foo.a", want: false},
		{fname: "obj/foo.d", want: false},
		{fname: "obj/foo.jar", want: false},
		// "cxx" without a leading dot must not match by extension.
		{fname: "gen/foocxx", want: false},
	} {
		if got := outputLocal(ctx, tc.fname); got != tc.want {
			t.Errorf("outputLocal(%q)=%t; want %t", tc.fname, got, tc.want)
		}
	}
}
