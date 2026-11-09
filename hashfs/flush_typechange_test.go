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
	"sync"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// TestFlush_TypeTransitions checks that Flush reconciles the on-disk entry with
// what hashfs records at a path, across every combination of file, symlink and
// directory (e.g. an incremental build where an output changed). For a type
// change the stale entry must be replaced -- otherwise os.WriteFile fails
// EISDIR, os.Symlink/os.Remove fails ENOTEMPTY, and os.MkdirAll fails
// ENOTDIR/EEXIST. Same-type cases exercise the in-place update paths: a file's
// contents and a symlink's target are refreshed, and a directory is kept (its
// stale members are left for dead-output cleanup, not cleared by flush).
func TestFlush_TypeTransitions(t *testing.T) {
	mtime := time.Unix(1000000000, 0)
	cmdhash := []byte("cmd")
	action := digest.Digest{Hash: "actionhash", SizeBytes: 10}
	const name = "gen/out"

	// putDisk materializes the given type at root/name on local disk, with a
	// non-empty directory so RemoveAll (not Remove) is exercised.
	putDisk := func(t *testing.T, root, typ string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		switch typ {
		case "file":
			if err := os.WriteFile(p, []byte("stale-file"), 0644); err != nil {
				t.Fatal(err)
			}
		case "symlink":
			if err := os.Symlink("stale-target", p); err != nil {
				t.Fatal(err)
			}
		case "dir":
			if err := os.MkdirAll(p, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, "child"), []byte("stale-child"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// record registers the intended type in hashfs at root/name. A directory is
	// recorded as a not-local (build-without-the-bytes) output, which
	// materializes at flush via flushDir.
	record := func(t *testing.T, hfs *HashFS, root, typ string) {
		t.Helper()
		ctx := t.Context()
		var err error
		switch typ {
		case "file":
			err = hfs.WriteFile(ctx, root, path.Path(name), []byte("new-file"), false, mtime, cmdhash, nil)
		case "symlink":
			err = hfs.Symlink(ctx, root, "new-target", path.Path(name), mtime, cmdhash, nil)
		case "dir":
			err = hfs.Update(ctx, root, []UpdateEntry{{
				Name:    name,
				Entry:   &merkletree.Entry{Name: name},
				Mode:    0o755 | fs.ModeDir,
				ModTime: mtime,
				CmdHash: cmdhash,
				Action:  action,
			}})
		}
		if err != nil {
			t.Fatalf("record %s: %v", typ, err)
		}
	}

	types := []string{"file", "symlink", "dir"}
	for _, disk := range types {
		for _, want := range types {
			t.Run(disk+"_on_disk_to_"+want, func(t *testing.T) {
				if runtime.GOOS == "windows" && (disk == "symlink" || want == "symlink") {
					t.Skip("no symlink on windows")
				}
				ctx := t.Context()
				root := t.TempDir()
				root, err := filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
				putDisk(t, root, disk)
				hfs, err := New(ctx, Option{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := hfs.Close(ctx); err != nil {
						t.Errorf("hfs.Close=%v", err)
					}
				})
				record(t, hfs, root, want)
				if err := hfs.Flush(ctx, root, []path.Path{path.Path(name)}); err != nil {
					t.Fatalf("Flush(%s on disk -> %s)=%v; want nil (stale %s must be replaced)", disk, want, err, disk)
				}
				p := filepath.Join(root, name)
				fi, err := os.Lstat(p)
				if err != nil {
					t.Fatalf("Lstat: %v", err)
				}
				switch want {
				case "file":
					if !fi.Mode().IsRegular() {
						t.Fatalf("on-disk mode=%v; want regular file", fi.Mode())
					}
					data, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					if got, want := string(data), "new-file"; got != want {
						t.Errorf("on-disk content=%q; want %q", got, want)
					}
				case "symlink":
					if fi.Mode()&os.ModeSymlink == 0 {
						t.Fatalf("on-disk mode=%v; want symlink", fi.Mode())
					}
					link, err := os.Readlink(p)
					if err != nil {
						t.Fatal(err)
					}
					if got, want := link, "new-target"; got != want {
						t.Errorf("on-disk symlink -> %q; want %q", got, want)
					}
				case "dir":
					if !fi.IsDir() {
						t.Fatalf("on-disk mode=%v; want directory", fi.Mode())
					}
					if disk == "dir" {
						// flushDir keeps an existing directory and does not clear
						// its contents (stale members are dead-output cleanup's
						// job, not flush's), so a pre-existing member is left
						// untouched.
						got, err := os.ReadFile(filepath.Join(p, "child"))
						if err != nil || string(got) != "stale-child" {
							t.Errorf("dir->dir: stale child = %q, err=%v; want %q left in place", got, err, "stale-child")
						}
					}
				}
			})
		}
	}
}

// TestFlush_StaleSymlinkSizeMtimeCoincidence covers the narrow case the
// size+mtime comparison alone cannot catch: a stale symlink whose Lstat size (the
// link-string length) and mtime both coincide with the recorded file output.
// The type check in matchesFileInfo must keep the flush from accepting the
// symlink as "already exist" and leaving the wrong entry type on disk.
func TestFlush_StaleSymlinkSizeMtimeCoincidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink on windows")
	}
	ctx := t.Context()
	cmdhash := []byte("cmd")
	const name = "gen/out"

	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	// Stale symlink whose target length equals the recorded content length.
	if err := os.Symlink("0123456789", p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
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
	// Record a regular-file output with size and mtime matching the symlink.
	content := []byte("abcdefghij")
	if got, want := int64(len(content)), fi.Size(); got != want {
		t.Fatalf("test setup: content size=%d; want %d (symlink target length)", got, want)
	}
	if err := hfs.WriteFile(ctx, root, name, content, false, fi.ModTime(), cmdhash, nil); err != nil {
		t.Fatalf("writefile: %v", err)
	}

	if err := hfs.Flush(ctx, root, []path.Path{path.Path(name)}); err != nil {
		t.Fatalf("Flush=%v; want nil", err)
	}
	lfi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !lfi.Mode().IsRegular() {
		t.Fatalf("on-disk mode=%v; want regular file (stale symlink must not pass as the recorded file)", lfi.Mode())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), string(content); got != want {
		t.Errorf("on-disk content=%q; want %q", got, want)
	}
}

// TestFlush_StaleFileAtParentDir covers a member flushed alone whose ancestor
// directory has become a stale regular file on disk after the entry was
// recorded (the parent's producing step is not re-run, so nothing else clears
// the path): the member's flush must replace the stale ancestor instead of
// failing MkdirAll with ENOTDIR.
func TestFlush_StaleFileAtParentDir(t *testing.T) {
	mtime := time.Unix(1000000000, 0)
	cmdhash := []byte("cmd")
	action := digest.Digest{Hash: "actionhash", SizeBytes: 10}

	for _, member := range []string{"file", "symlink", "dir"} {
		t.Run(member, func(t *testing.T) {
			if runtime.GOOS == "windows" && member == "symlink" {
				t.Skip("no symlink on windows")
			}
			ctx := t.Context()
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
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

			const name = "gen/sub/out"
			switch member {
			case "file":
				if err := hfs.WriteFile(ctx, root, name, []byte("data"), false, mtime, cmdhash, nil); err != nil {
					t.Fatalf("writefile: %v", err)
				}
			case "symlink":
				if err := hfs.Symlink(ctx, root, "target", name, mtime, cmdhash, nil); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			case "dir":
				if err := hfs.Update(ctx, root, []UpdateEntry{{
					Name:    name,
					Entry:   &merkletree.Entry{Name: name},
					Mode:    0o755 | fs.ModeDir,
					ModTime: mtime,
					CmdHash: cmdhash,
					Action:  action,
				}}); err != nil {
					t.Fatalf("update dir: %v", err)
				}
			}

			// After recording, the on-disk parent is replaced by a stale
			// regular file (e.g. mangled out dir, or a leftover from a
			// previous build whose producing step is skipped this build).
			if err := os.WriteFile(filepath.Join(root, "gen"), []byte("stale"), 0644); err != nil {
				t.Fatal(err)
			}

			if err := hfs.Flush(ctx, root, []path.Path{path.Path(name)}); err != nil {
				t.Fatalf("Flush(%q under stale-file ancestor)=%v; want nil", member, err)
			}
			fi, err := os.Lstat(filepath.Join(root, "gen"))
			if err != nil {
				t.Fatal(err)
			}
			if !fi.IsDir() {
				t.Fatalf("gen mode=%v; want directory (stale file ancestor replaced)", fi.Mode())
			}
			p := filepath.Join(root, name)
			switch member {
			case "file":
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := string(b), "data"; got != want {
					t.Errorf("on-disk content=%q; want %q", got, want)
				}
			case "symlink":
				got, err := os.Readlink(p)
				if err != nil {
					t.Fatal(err)
				}
				if want := "target"; got != want {
					t.Errorf("on-disk symlink target=%q; want %q", got, want)
				}
			case "dir":
				fi, err := os.Lstat(p)
				if err != nil {
					t.Fatal(err)
				}
				if !fi.IsDir() {
					t.Errorf("on-disk mode=%v; want directory", fi.Mode())
				}
			}
		})
	}
}

// TestFlush_UnstatableSymlinkAncestorKept covers an ancestor that is a
// symlink whose target cannot be statted (here EACCES; EIO in the wild).
// mkdirAllForFlush must not treat it as stale: only a symlink that resolves
// to a non-directory or is dangling is a leftover. Destroying an intentional
// symlink would replace it with a real directory and silently divert outputs
// from the symlink's target.
func TestFlush_UnstatableSymlinkAncestorKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink on windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	mtime := time.Unix(1000000000, 0)
	cmdhash := []byte("cmd")
	ctx := t.Context()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
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

	// gen is an intentional symlink to a directory; its target becomes
	// unreachable (parent directory unsearchable), so stat through the
	// symlink fails with EACCES.
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "real"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("locked/real", filepath.Join(root, "gen")); err != nil {
		t.Fatal(err)
	}
	if err := hfs.WriteFile(ctx, root, "gen/sub/out", []byte("data"), false, mtime, cmdhash, nil); err != nil {
		t.Fatalf("WriteFile=%v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o755); err != nil {
			t.Error(err)
		}
	})

	if err := hfs.Flush(ctx, root, []path.Path{"gen/sub/out"}); err == nil {
		t.Error("Flush=nil; want error (target unreachable, symlink must not be classified as stale)")
	}
	fi, err := os.Lstat(filepath.Join(root, "gen"))
	if err != nil {
		t.Fatalf("Lstat(gen)=%v; want the symlink kept", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("gen mode=%v; want symlink (mkdirAllForFlush must not replace an unstatable symlink with a directory)", fi.Mode())
	}
}

// TestFlush_ConcurrentStaleAncestorRemoval flushes two sibling outputs below
// the same stale-file ancestor concurrently. The removal must be serialized:
// without it, both flushes can observe the stale file, one recreates the
// directory and materializes its output, and the other then RemoveAlls the
// directory based on its stale Lstat, deleting the sibling's fresh output
// (or failing the flush with "directory not empty").
func TestFlush_ConcurrentStaleAncestorRemoval(t *testing.T) {
	mtime := time.Unix(1000000000, 0)
	cmdhash := []byte("cmd")
	ctx := t.Context()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
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

	for i := range 200 {
		gen := fmt.Sprintf("gen%d", i)
		outs := []string{gen + "/sub/a", gen + "/sub/b"}
		for _, f := range outs {
			if err := hfs.WriteFile(ctx, root, path.Path(f), []byte("data-"+f), false, mtime, cmdhash, nil); err != nil {
				t.Fatalf("WriteFile(%q)=%v", f, err)
			}
		}
		// A stale regular file sits where the outputs' ancestor directory
		// belongs, as if left behind by a previous build.
		if err := os.WriteFile(filepath.Join(root, gen), []byte("stale"), 0644); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		errs := make([]error, len(outs))
		var wg sync.WaitGroup
		for j, f := range outs {
			wg.Go(func() {
				<-start
				errs[j] = hfs.Flush(ctx, root, []path.Path{path.Path(f)})
			})
		}
		close(start)
		wg.Wait()
		for j, err := range errs {
			if err != nil {
				t.Fatalf("iter %d: Flush(%q)=%v; want nil", i, outs[j], err)
			}
		}
		for _, f := range outs {
			b, err := os.ReadFile(filepath.Join(root, f))
			if err != nil {
				t.Fatalf("iter %d: ReadFile(%q)=%v; want output on disk (concurrent stale-ancestor removal deleted a sibling's fresh output?)", i, f, err)
			}
			if got, want := string(b), "data-"+f; got != want {
				t.Fatalf("iter %d: %q content=%q; want %q", i, f, got, want)
			}
		}
	}
}
