// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
)

// newMatchDirFS creates this tree under a temp dir and returns it with a
// new HashFS that has not loaded anything yet:
//
//	a/f  file "f"
//	a/g  file "g", mode 0755 (not executable on Windows)
//	a/d/ directory
//	a/e/ directory
//	a/s  symlink to f
//	l    symlink to a
func newMatchDirFS(t *testing.T) (*hashfs.HashFS, string) {
	t.Helper()
	// MatchDir returns false below a symlink, so the root must not have
	// a symlink ancestor (e.g. /var/folders on macOS).
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"a/d", "a/e"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a/f"), []byte("f"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a/g"), []byte("g"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(root, "a/s")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(t.Context(), hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hfs.Close(t.Context()) })
	return hfs, root
}

// loadMatchDirTree loads the tree through Entries, as matchInputRoot's
// slow path does. a/missing does not exist, so hashfs records it as
// missing.
func loadMatchDirTree(t *testing.T, hfs *hashfs.HashFS, root string) {
	t.Helper()
	names := []string{"a/f", "a/g", "a/d", "a/e", "a/s", "a/missing", "l"}
	if _, err := hfs.Entries(t.Context(), root, path.Paths(names)); err != nil {
		t.Fatal(err)
	}
}

// fileChild returns the DirChild that matches what hashfs records for the
// file name. The executable bit comes from hashfs, not from the mode the
// test wrote, because Windows has no executable bits.
func fileChild(t *testing.T, hfs *hashfs.HashFS, root, name string) hashfs.DirChild {
	t.Helper()
	ents, err := hfs.Entries(t.Context(), root, path.Paths([]string{name}))
	if err != nil || len(ents) != 1 || ents[0].Data.Digest().IsZero() {
		t.Fatalf("Entries(%q) = %v, %v; want one file", name, ents, err)
	}
	return hashfs.DirChild{
		Name:         filepath.Base(name),
		Digest:       ents[0].Data.Digest(),
		IsExecutable: ents[0].IsExecutable,
	}
}

func TestMatchDir(t *testing.T) {
	ctx := t.Context()
	hfs, root := newMatchDirFS(t)
	d := hashfs.DirChild{Name: "d"}

	if hfs.MatchDir(ctx, hashfs.MakeFullpath(root, "a"), []hashfs.DirChild{d}) {
		t.Errorf("MatchDir before anything was loaded = true; want false")
	}

	loadMatchDirTree(t, hfs, root)
	f := fileChild(t, hfs, root, "a/f")
	g := fileChild(t, hfs, root, "a/g")
	if f.Digest == g.Digest {
		t.Fatalf("f and g have the same digest %v", f.Digest)
	}

	for _, tc := range []struct {
		name     string
		dir      string // relative to root
		children []hashfs.DirChild
		want     bool
	}{
		{
			// e and s are not listed: other children don't matter.
			name:     "all_match",
			dir:      "a",
			children: []hashfs.DirChild{f, g, d},
			want:     true,
		},
		{
			name: "no_children",
			dir:  "a",
			want: true,
		},

		// A file must have exactly the wanted digest and executable
		// bit. g's digest is just some other real digest.
		{
			name:     "f_with_g_digest",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "f", Digest: g.Digest, IsExecutable: f.IsExecutable}},
		},
		{
			name:     "f_executable_bit_flipped",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "f", Digest: f.Digest, IsExecutable: !f.IsExecutable}},
		},
		{
			name:     "g_executable_bit_flipped",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "g", Digest: g.Digest, IsExecutable: !g.IsExecutable}},
		},

		// A zero digest means "want a directory".
		{
			name:     "file_wanted_as_dir",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "f"}},
		},
		{
			name:     "dir_wanted_as_file",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "d", Digest: f.Digest}},
		},

		// MatchDir only says true for loaded files and directories.
		{
			name:     "symlink_wanted_as_file",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "s", Digest: f.Digest}},
		},
		{
			name:     "symlink_wanted_as_dir",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "s"}},
		},
		{
			name:     "child_recorded_as_missing",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "missing", Digest: f.Digest}},
		},
		{
			name:     "child_never_loaded",
			dir:      "a",
			children: []hashfs.DirChild{{Name: "never", Digest: f.Digest}},
		},

		// The directory itself must be loaded and not reached through
		// a symlink.
		{
			name:     "dir_through_symlink",
			dir:      "l",
			children: []hashfs.DirChild{f, g, d},
		},
		{
			name:     "unknown_dir",
			dir:      "nope",
			children: []hashfs.DirChild{f, g, d},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fulldir := hashfs.MakeFullpath(root, tc.dir)
			if got := hfs.MatchDir(ctx, fulldir, tc.children); got != tc.want {
				t.Errorf("MatchDir(%q, %v) = %t; want %t", fulldir, tc.children, got, tc.want)
			}
		})
	}
}

// TestMatchDirAfterMutation checks that changes made through hashfs are
// seen by the next MatchDir call.
func TestMatchDirAfterMutation(t *testing.T) {
	ctx := t.Context()
	hfs, root := newMatchDirFS(t)
	loadMatchDirTree(t, hfs, root)
	fulldir := hashfs.MakeFullpath(root, "a")
	oldF := fileChild(t, hfs, root, "a/f")
	g := fileChild(t, hfs, root, "a/g")
	d := hashfs.DirChild{Name: "d"}

	check := func(name string, children []hashfs.DirChild, want bool) {
		t.Helper()
		if got := hfs.MatchDir(ctx, fulldir, children); got != want {
			t.Errorf("%s: MatchDir(%q, %v) = %t; want %t", name, fulldir, children, got, want)
		}
	}

	err := hfs.WriteFile(ctx, root, "a/f", []byte("new"), false, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	check("old f after WriteFile", []hashfs.DirChild{oldF}, false)
	check("new f after WriteFile", []hashfs.DirChild{fileChild(t, hfs, root, "a/f")}, true)

	if err := hfs.RemoveAll(ctx, root, "a/d"); err != nil {
		t.Fatal(err)
	}
	check("d after RemoveAll a/d", []hashfs.DirChild{d}, false)
	check("g after RemoveAll a/d", []hashfs.DirChild{g}, true)

	if err := hfs.RemoveAll(ctx, root, "a"); err != nil {
		t.Fatal(err)
	}
	check("g after RemoveAll a", []hashfs.DirChild{g}, false)
}

// TestMatchDirDigestError checks that MatchDir does not race with a digest
// that fails while it looks at the entry (run with -race).
func TestMatchDirDigestError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read an unreadable file")
	}
	ctx := t.Context()
	root := t.TempDir()
	full := filepath.Join(root, "a/u")
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	// Stat succeeds, but reading it to compute the digest fails.
	if err := os.WriteFile(full, []byte("u"), 0); err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hfs.Close(ctx) })
	if _, err := hfs.Stat(ctx, root, "a/u"); err != nil {
		t.Fatal(err)
	}

	fulldir := hashfs.MakeFullpath(root, "a")
	want := []hashfs.DirChild{{Name: "u", Digest: digest.Digest{Hash: "e", SizeBytes: 1}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Computes the digest, which fails.
		hfs.Entries(ctx, root, path.Paths([]string{"a/u"}))
	}()
	// Keep calling MatchDir while the digest fails, so that some call
	// reads the entry right after the failure is recorded.
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		if hfs.MatchDir(ctx, fulldir, want) {
			t.Fatal("MatchDir = true for a file without digest; want false")
		}
	}
	if hfs.MatchDir(ctx, fulldir, want) {
		t.Error("MatchDir after a failed digest = true; want false")
	}
}
