// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sisopath "go.chromium.org/build/siso/path"
)

func TestPath_FromRelative(t *testing.T) {
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
		got, err := path.FromRelative(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("path.FromRelative(%q)=%q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
}

func TestPath_FromRelativePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink/usrmerge path semantics")
	}
	ctx := t.Context()
	dir := t.TempDir()
	// Absolute path outside the workspace whose ".." crosses a symlink; it
	// must be preserved verbatim, not lexically cleaned. Concatenate, not
	// filepath.Join, so the ".." survives into the input.
	crossSymlink := t.TempDir() + "/lib/../arm-linux-gnueabihf/include/bits/long-double.h"
	p := NewPath(dir, "out/siso")
	for _, tc := range []struct {
		in   string
		want string
	}{
		// Workspace-relative inputs are cleaned like any Path.
		{in: "foo", want: "out/siso/foo"},
		{in: "foo/./bar", want: "out/siso/foo/bar"},
		{in: "../../foo/bar", want: "foo/bar"},
		{in: filepath.Join(dir, "foo/bar"), want: "foo/bar"},
		// Absolute out-of-workspace path kept verbatim; ".." not collapsed.
		{in: crossSymlink, want: crossSymlink},
		// Leading-slash path that begins with "..": kept verbatim, not
		// collapsed to /include/bla.h.
		{in: "/../include/bla.h", want: "/../include/bla.h"},
	} {
		got := p.FromRelativePath(ctx, tc.in)
		if string(got) != tc.want {
			t.Errorf("FromRelativePath(%q)=%q; want %q", tc.in, got, tc.want)
		}
	}

	// FromRelativePath must not lexically collapse the "..", as path.New does.
	if got := p.FromRelativePath(ctx, crossSymlink); got == sisopath.New(crossSymlink) {
		t.Errorf("FromRelativePath(%q)=%q was lexically cleaned; want verbatim", crossSymlink, got)
	}
}

func TestPath_FromRelative_Windows(t *testing.T) {
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
		got, err := path.FromRelative(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("path.FromRelative(%q)=%q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
}

func TestPath_FromRelativePath_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("These tests are Windows only")
	}
	ctx := t.Context()
	p := NewPath(t.TempDir(), "out\\siso")
	// Out-of-workspace absolute native path with backslashes and "..":
	// separators must normalize to "/", but ".." must survive uncollapsed so
	// the OS can resolve it.
	in := `C:\SDK\lib\..\include\foo.h`
	got := p.FromRelativePath(ctx, in)
	if want := sisopath.Path("C:/SDK/lib/../include/foo.h"); got != want {
		t.Errorf("FromRelativePath(%q)=%q; want %q", in, got, want)
	}
	if !got.IsAbs() {
		t.Errorf("FromRelativePath(%q)=%q; IsAbs()=false, want true", in, got)
	}
}

// TestPath_MaybeToRelative_SlashSeparated pins the documented contract:
// results are slash-separated on every host. filepath.Rel returns
// OS-native separators, so on Windows a nested path must be converted
// back to slashes before it reaches ninja node lookups and deps
// recording, which key on slash paths.
func TestPath_MaybeToRelative_SlashSeparated(t *testing.T) {
	ctx := t.Context()
	p := NewPath("/workspace", "out/Default")
	got := p.MaybeToRelative(ctx, "out/Default/obj/base/foo.o")
	if want := "obj/base/foo.o"; got != want {
		t.Errorf("MaybeToRelative(out/Default/obj/base/foo.o) = %q, want %q", got, want)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("MaybeToRelative returned OS-native separators: %q", got)
	}
}
