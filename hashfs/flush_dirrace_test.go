// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// TestFlush_LocalReadyDirInputNotFlushed verifies Flush leaves a local-ready
// directory alone. prepareLocalInputs flushes inputs too, so a source directory
// input reaches Flush; materializing it (flushDir) could Chtimes-reset it or,
// when the on-disk type changed, RemoveAll a source path as if it were a stale
// output. A recorded directory whose on-disk path was replaced by a file must
// be left in place, not treated as a stale output and removed.
func TestFlush_LocalReadyDirInputNotFlushed(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "srcdir")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hfs.Close(ctx); err != nil {
			t.Errorf("hfs.Close=%v", err)
		}
	})
	// Stat records srcdir as a local-ready directory input.
	if _, err := hfs.Stat(ctx, root, "srcdir"); err != nil {
		t.Fatalf("stat: %v", err)
	}

	// The on-disk source is replaced by a file after being recorded.
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("now-a-file"), 0644); err != nil {
		t.Fatal(err)
	}
	fileMtime := time.Unix(1700000000, 0)
	if err := os.Chtimes(src, time.Time{}, fileMtime); err != nil {
		t.Fatal(err)
	}

	if err := hfs.Flush(ctx, root, []path.Path{"srcdir"}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got, err := os.ReadFile(src)
	if err != nil || string(got) != "now-a-file" {
		t.Errorf("source after flush = %q, err=%v; want %q left in place (local-ready input not flushed as a stale output)", got, err, "now-a-file")
	}
	fi, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fi.ModTime(), fileMtime; !got.Equal(want) {
		t.Errorf("%q mtime after flush = %v; want %v (skipped local-ready entry must not be reset to the recorded dir mtime)", src, got, want)
	}
}

// TestFlush_LocalReadyDirMtimeNotRestored verifies Flush does not rewrite the
// on-disk mtime of a local-ready source directory it skipped. Resetting it to
// the recorded mtime would make updateDir's mtime comparison see no change, so
// a file the user added mid-build would stay invisible to ReadDir/Glob.
func TestFlush_LocalReadyDirMtimeNotRestored(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "srcdir")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hfs.Close(ctx); err != nil {
			t.Errorf("hfs.Close=%v", err)
		}
	})
	// Stat records srcdir as a local-ready directory input.
	if _, err := hfs.Stat(ctx, root, "srcdir"); err != nil {
		t.Fatalf("stat: %v", err)
	}

	// The user adds a file mid-build, bumping the directory mtime.
	if err := os.WriteFile(filepath.Join(src, "g"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}
	bumped := time.Unix(1700000000, 0)
	if err := os.Chtimes(src, time.Time{}, bumped); err != nil {
		t.Fatal(err)
	}

	if err := hfs.Flush(ctx, root, []path.Path{"srcdir"}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	fi, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fi.ModTime(), bumped; !got.Equal(want) {
		t.Errorf("dir mtime after flush = %v; want %v (skipped local-ready dir must not be reset to the recorded mtime)", got, want)
	}
}

// TestFlush_PopulatedDirReplacesStaleNonDir covers a directory output whose
// path is still a stale file or symlink on disk from a previous build. Flush
// expands a trailing-slash directory into its members and flushes them
// concurrently; each member's flush MkdirAll's its parent. The parent directory
// must be materialized (stale entry removed, directory created) before any
// member flush runs, or a member races ahead and MkdirAll fails ENOTDIR (or
// follows the stale symlink). Many members make the race reliable.
//
// The membersFirst order lists every member before the directory target, as
// happens when a file output is declared under a directory output (builder.go
// appends file outputs ahead of dir+"/" outputs). expandFlushDirs must sort
// the parent directory ahead of its members regardless of caller order.
func TestFlush_PopulatedDirReplacesStaleNonDir(t *testing.T) {
	mtime := time.Unix(1000000000, 0)
	cmdhash := []byte("cmd")
	action := digest.Digest{Hash: "actionhash", SizeBytes: 10}
	const dir = "gen/out"
	const nchild = 24

	for _, disk := range []string{"file", "symlink"} {
		for _, order := range []string{"dirTarget", "membersFirst"} {
			t.Run(disk+"_"+order, func(t *testing.T) {
				if runtime.GOOS == "windows" && disk == "symlink" {
					t.Skip("no symlink on windows")
				}
				ctx := t.Context()
				root := t.TempDir()
				root, err := filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(root, dir)
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				switch disk {
				case "file":
					if err := os.WriteFile(p, []byte("stale-file"), 0644); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink("stale-target", p); err != nil {
						t.Fatal(err)
					}
				}

				hfs, err := New(ctx, Option{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := hfs.Close(ctx); err != nil {
						t.Errorf("hfs.Close=%v", err)
					}
				})

				// Record the directory as a not-local (build-without-the-bytes)
				// output, plus many member files under it.
				if err := hfs.Update(ctx, root, []UpdateEntry{{
					Name:    dir,
					Entry:   &merkletree.Entry{Name: dir},
					Mode:    0o755 | fs.ModeDir,
					ModTime: mtime,
					CmdHash: cmdhash,
					Action:  action,
				}}); err != nil {
					t.Fatalf("update dir: %v", err)
				}
				var children []string
				for i := range nchild {
					child := fmt.Sprintf("%s/child%02d", dir, i)
					if err := hfs.WriteFile(ctx, root, path.Path(child), []byte("c"), false, mtime, cmdhash, nil); err != nil {
						t.Fatalf("writefile %q: %v", child, err)
					}
					children = append(children, child)
				}

				var files []string
				switch order {
				case "dirTarget":
					files = []string{dir + "/"}
				case "membersFirst":
					files = append(children, dir+"/")
				}
				if err := hfs.Flush(ctx, root, path.Paths(files)); err != nil {
					t.Fatalf("Flush(%s over stale %s)=%v; want nil", order, disk, err)
				}

				fi, err := os.Lstat(p)
				if err != nil {
					t.Fatalf("Lstat(%q): %v", p, err)
				}
				if !fi.IsDir() {
					t.Fatalf("%q: on-disk mode=%v; want directory", p, fi.Mode())
				}
				for i := range nchild {
					cp := filepath.Join(p, fmt.Sprintf("child%02d", i))
					b, err := os.ReadFile(cp)
					if err != nil || string(b) != "c" {
						t.Errorf("child%02d = %q, err=%v; want %q", i, b, err, "c")
					}
				}
			})
		}
	}
}
