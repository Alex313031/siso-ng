// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestPath_FromWD(t *testing.T) {
	dir := t.TempDir()
	absPath := filepath.Join(t.TempDir(), "test")
	path := NewPath(dir, "out/siso")
	for _, tc := range []struct {
		in   string
		want string
	}{
		{
			in:   "foo",
			want: "out/siso/foo",
		},
		{
			in:   "foo/bar",
			want: "out/siso/foo/bar",
		},
		{
			in:   "../../foo/bar",
			want: "foo/bar",
		},
		{
			in:   filepath.Join(dir, "foo/bar"),
			want: "foo/bar",
		},
		{
			in:   absPath,
			want: absPath,
		},
	} {
		got, err := path.FromWD(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("path.FromWD(%q)=%q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
}

func TestPath_FromWD_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("These tests are Windows only")
	}

	dir := t.TempDir()
	absPath := filepath.Join(t.TempDir(), "test")
	path := NewPath(dir, "out\\siso")
	for _, tc := range []struct {
		in   string
		want string
	}{
		{
			in:   "foo",
			want: "out/siso/foo",
		},
		{
			in:   "foo\\bar",
			want: "out/siso/foo/bar",
		},
		{
			in:   "..\\..\\foo\\bar",
			want: "foo/bar",
		},
		{
			in:   filepath.Join(dir, "foo\\bar"),
			want: "foo/bar",
		},
		{
			in:   absPath,
			want: absPath,
		},
	} {
		got, err := path.FromWD(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("path.FromWD(%q)=%q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
}
