// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// TestDirectoryDeleteKeepsPopulatedDir is a regression test for the racing
// "missing outputs" failure fixed in commit 17c8f73e.
//
// In racing mode a step's RecordOutputsFromLocal -> RetrieveUpdateEntriesFromLocal
// clears the negative cache of parent directories by calling directory.delete on
// ancestor dir nodes. Deleting a directory node also evicts all of its children,
// so when a concurrent racing step has recorded a remote-won (is_local=false)
// output in the same directory, that sibling entry lives under the same hashfs
// dir node and would be orphaned -- b.outputs() can then no longer Stat it and
// the step fails with a spurious "missing outputs".
//
// directory.delete must never evict a directory node that still has children.
func TestDirectoryDeleteKeepsPopulatedDir(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	// A concurrent racing step recorded a remote-won output (is_local=false,
	// digest only -- not written to local disk) under outdir/. This creates a
	// virtual "outdir" dir node in hashfs holding the sibling entry.
	sibling := "outdir/sibling.o"
	sd := digest.Digest{Hash: "siblinghash", SizeBytes: 4}
	err = hfs.Update(ctx, dir, []UpdateEntry{{
		Name:    sibling,
		Entry:   &merkletree.Entry{Name: sibling, Data: digest.NewData(nil, sd)},
		Mode:    0o644,
		CmdHash: []byte("cmdhash"),
	}})
	if err != nil {
		t.Fatalf("Update(%q): %v", sibling, err)
	}
	if _, err := hfs.Stat(ctx, dir, sibling); err != nil {
		t.Fatalf("Stat(%q) before delete: %v; want nil", sibling, err)
	}

	// This is exactly what RetrieveUpdateEntriesFromLocal's parent-directory
	// negative-cache clearing does while a different step records its own local
	// output in outdir/: directory.delete on the parent dir node, which keeps a
	// populated directory.
	hfs.directory.delete(ctx, filepath.ToSlash(filepath.Join(dir, "outdir")))

	// The remote-won sibling must survive: deleting a populated dir node is
	// never a valid cache invalidation.
	if _, err := hfs.Stat(ctx, dir, sibling); err != nil {
		t.Errorf("Stat(%q) after directory.delete(outdir) = %v; want nil (sibling was orphaned)", sibling, err)
	}
}

// TestDirectoryDeleteKeepsEmptyDir is a regression test for the racing orphan
// window the populated-dir guard does not close: directory.delete must never evict
// a directory node, not even an empty one.
//
// In storeEntry, Update(outdir/sibling.o) publishes the outdir directory node into
// its parent map (storeNextDir) BEFORE it stores sibling.o under it. If a concurrent
// parent-cache-clearing directory.delete(outdir) runs in that gap, the old guard's
// Range sees no children and unlinks outdir; the in-flight Update then stores
// sibling.o into an orphaned directory, so the "missing outputs" failure recurs. The
// fix is to never evict a directory node (the empty-vs-populated check is racy);
// real directory removal goes through deleteForce/RemoveAll.
func TestDirectoryDeleteKeepsEmptyDir(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	// Build an empty virtual directory node: record a child (remote-won, not on
	// local disk) to create out/sub, then force-remove the child so out/sub is an
	// empty directory node that exists only in hashfs (not on disk). A surviving
	// Stat(out/sub) therefore proves the entry itself was not evicted.
	child := "out/sub/child"
	cd := digest.Digest{Hash: "childhash", SizeBytes: 4}
	err = hfs.Update(ctx, dir, []UpdateEntry{{
		Name:    child,
		Entry:   &merkletree.Entry{Name: child, Data: digest.NewData(nil, cd)},
		Mode:    0o644,
		CmdHash: []byte("cmdhash"),
	}})
	if err != nil {
		t.Fatalf("Update(%q): %v", child, err)
	}
	hfs.directory.deleteForce(ctx, filepath.ToSlash(filepath.Join(dir, child)))
	outSub := filepath.ToSlash(filepath.Join(dir, "out/sub"))
	if e, _, _, ok := hfs.directory.lookup(ctx, outSub); !ok || e == nil || e.getDir() == nil {
		t.Fatalf("precondition: out/sub is not an empty in-memory dir node (ok=%v)", ok)
	}

	// Clearing a parent's negative cache must not evict the (empty) directory node.
	hfs.directory.delete(ctx, outSub)

	if e, _, _, ok := hfs.directory.lookup(ctx, outSub); !ok || e == nil || e.getDir() == nil {
		t.Errorf("out/sub dir node evicted by directory.delete (ok=%v); want it preserved", ok)
	}
}

// TestForgetMissingsInDir_PrunesUnderMissingGeneratedDir is a regression test for
// the deleteNotGenerated path: when a generated output directory is itself gone on
// local disk, its stale non-generated children must still be pruned. The generated
// check must apply only to leaves, not skip the whole directory before recursing.
// Covers both entry points: ForgetMissingsInDir on the generated dir directly, and
// on its parent (so deleteNotGeneratedLeaves recurses into a generated subdir).
func TestForgetMissingsInDir_PrunesUnderMissingGeneratedDir(t *testing.T) {
	for _, target := range []string{"out/gendir", "out"} {
		t.Run(target, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			dir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			hfs, err := New(ctx, Option{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer hfs.Close(ctx)

			// out/gendir is a generated output dir (cmdhash) now GONE on local disk;
			// out/gendir/stale is a non-generated child cached under it. Neither is
			// written to disk.
			if err := hfs.Update(ctx, dir, []UpdateEntry{{
				Name:    "out/gendir",
				Entry:   &merkletree.Entry{Name: "out/gendir"},
				Mode:    0o755 | fs.ModeDir,
				CmdHash: []byte("dircmdhash"),
			}}); err != nil {
				t.Fatalf("Update(out/gendir): %v", err)
			}
			stale := "out/gendir/stale"
			sd := digest.Digest{Hash: "stalehash", SizeBytes: 4}
			if err := hfs.Update(ctx, dir, []UpdateEntry{{
				Name:  stale,
				Entry: &merkletree.Entry{Name: stale, Data: digest.NewData(nil, sd)},
				Mode:  0o644,
			}}); err != nil {
				t.Fatalf("Update(%q): %v", stale, err)
			}
			staleFull := filepath.ToSlash(filepath.Join(dir, stale))
			if _, _, _, ok := hfs.directory.lookup(ctx, staleFull); !ok {
				t.Fatalf("precondition: %s not in hashfs", stale)
			}

			hfs.ForgetMissingsInDir(ctx, dir, target)

			if _, _, _, ok := hfs.directory.lookup(ctx, staleFull); ok {
				t.Errorf("ForgetMissingsInDir(%q) left stale child %s reachable under a missing generated dir", target, stale)
			}
			// The generated dir node itself stays anchored (never unlinked).
			genFull := filepath.ToSlash(filepath.Join(dir, "out/gendir"))
			if e, _, _, ok := hfs.directory.lookup(ctx, genFull); !ok || e.getDir() == nil {
				t.Errorf("ForgetMissingsInDir(%q) evicted the generated dir node out/gendir (ok=%v); want it preserved", target, ok)
			}
		})
	}
}

// TestForgetMissingsInDir_ReconcilesGeneratedDir is a regression test for the
// b/350662100 reconcile path: ForgetMissingsInDir on a generated output directory
// (cmdhash-stamped, like a recorded output dir) that still exists on disk must
// descend into it and prune children the step removed on disk. Skipping the whole
// directory just because its own entry is generated leaves stale removed files
// reachable.
func TestForgetMissingsInDir_ReconcilesGeneratedDir(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	// out/gendir is a generated output directory (cmdhash, as recorded output dirs
	// are - execute/cmd.go), present on local disk.
	if err := os.MkdirAll(filepath.Join(dir, "out/gendir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := hfs.Update(ctx, dir, []UpdateEntry{{
		Name:    "out/gendir",
		Entry:   &merkletree.Entry{Name: "out/gendir"},
		Mode:    0o755 | fs.ModeDir,
		CmdHash: []byte("dircmdhash"),
	}}); err != nil {
		t.Fatalf("Update(out/gendir): %v", err)
	}
	// A non-generated file the step removed: cached under gendir, gone on disk.
	stale := "out/gendir/stale"
	sd := digest.Digest{Hash: "stalehash", SizeBytes: 4}
	if err := hfs.Update(ctx, dir, []UpdateEntry{{
		Name:  stale,
		Entry: &merkletree.Entry{Name: stale, Data: digest.NewData(nil, sd)},
		Mode:  0o644,
	}}); err != nil {
		t.Fatalf("Update(%q): %v", stale, err)
	}
	staleFull := filepath.ToSlash(filepath.Join(dir, stale))
	if _, _, _, ok := hfs.directory.lookup(ctx, staleFull); !ok {
		t.Fatalf("precondition: %s not in hashfs", stale)
	}

	hfs.ForgetMissingsInDir(ctx, dir, "out/gendir")

	if _, _, _, ok := hfs.directory.lookup(ctx, staleFull); ok {
		t.Errorf("ForgetMissingsInDir(out/gendir) left stale removed file %s reachable; the generated dir was not reconciled", stale)
	}
}

// TestForgetOutputsDropsOutputDirChildren verifies the exact-output invalidation
// path used after a racing remote win: declared output directories are owned by
// the step, so stale children from the canceled local racer must be forgotten.
func TestForgetOutputsDropsOutputDirChildren(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	child := "out/sub/child"
	cd := digest.Digest{Hash: "childhash", SizeBytes: 4}
	err = hfs.Update(ctx, dir, []UpdateEntry{{
		Name:    child,
		Entry:   &merkletree.Entry{Name: child, Data: digest.NewData(nil, cd)},
		Mode:    0o644,
		CmdHash: []byte("cmdhash"),
	}})
	if err != nil {
		t.Fatalf("Update(%q): %v", child, err)
	}
	childFull := filepath.ToSlash(filepath.Join(dir, child))
	if _, _, _, ok := hfs.directory.lookup(ctx, childFull); !ok {
		t.Fatalf("precondition: %s not in hashfs", child)
	}

	hfs.ForgetOutputs(ctx, dir, []string{"out"})

	if _, _, _, ok := hfs.directory.lookup(ctx, childFull); ok {
		t.Errorf("ForgetOutputs(out) left stale child %s reachable", child)
	}
}

// TestRetrieveUpdateEntriesFromLocal_RemovedDirDropsChildren is a regression test
// for the review follow-up to the Mode A guard: the child-preserving delete must
// not be used when the exact path itself disappeared from local disk.
//
// When a local action removes an output directory that hashfs still caches as a
// populated directory, RetrieveUpdateEntriesFromLocal sees Lstat(out/sub) ==
// ErrNotExist for the path itself and must drop the whole stale subtree. The
// child-preserving delete early-returns on a populated dir, leaving cached children
// (e.g. out/sub/child) reachable via Stat/ReadDir even though out/sub is gone. The
// "keep populated dir" guard is only correct for ancestor negative-cache clearing
// (where the parent still exists on disk); an exact-path removal needs deleteForce.
func TestRetrieveUpdateEntriesFromLocal_RemovedDirDropsChildren(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	// hashfs caches out/sub as a populated directory (here via a recorded child,
	// not written to local disk), but out/sub does not exist on local disk - the
	// action that produced it removed it.
	child := "out/sub/child"
	cd := digest.Digest{Hash: "childhash", SizeBytes: 4}
	err = hfs.Update(ctx, dir, []UpdateEntry{{
		Name:    child,
		Entry:   &merkletree.Entry{Name: child, Data: digest.NewData(nil, cd)},
		Mode:    0o644,
		CmdHash: []byte("cmdhash"),
	}})
	if err != nil {
		t.Fatalf("Update(%q): %v", child, err)
	}
	if _, err := hfs.Stat(ctx, dir, child); err != nil {
		t.Fatalf("Stat(%q) precondition: %v; want nil", child, err)
	}

	// Record local outputs as RecordOutputsFromLocal does: out/sub is gone on disk
	// (ErrNotExist for the path itself), so its stale cached subtree must go.
	hfs.RetrieveUpdateEntriesFromLocal(ctx, dir, []string{"out/sub"})

	// The cached child must no longer be reachable: out/sub was removed, so a Stat
	// of out/sub/child must report it gone, not return the stale entry.
	if _, err := hfs.Stat(ctx, dir, child); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%q) after removing out/sub = %v; want fs.ErrNotExist (stale child survived)", child, err)
	}
}

// TestForgetMissingsInDir_RemovedDirDropsChildren is a regression test for the
// review follow-up: when a cached subdirectory is removed on local disk,
// ForgetMissingsInDir must drop stale cached children under it.
//
// The subtlety is that ForgetMissingsInDir gates every path through hfs.Stat, which
// for a directory does an internal disk Lstat and returns ErrNotExist when the dir
// is gone - so a removed subdir is skipped before reaching the per-file Lstat/delete
// loop, and Stat does not itself drop the cached children, leaving them
// reachable. The fix prunes stale non-generated leaves at that Stat==ErrNotExist
// point while keeping directory nodes anchored for concurrent child stores.
func TestForgetMissingsInDir_RemovedDirDropsChildren(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	// out/sub is a real on-disk directory with a child; cache it, then remove the
	// subdirectory from disk (as a step that rewrites an output dir would).
	if err := os.MkdirAll(filepath.Join(dir, "out/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out/sub/child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.ReadDir(ctx, dir, "out/sub"); err != nil {
		t.Fatalf("ReadDir(out/sub): %v", err)
	}
	if _, err := hfs.Stat(ctx, dir, "out/sub/child"); err != nil {
		t.Fatalf("Stat(out/sub/child) precondition: %v; want nil", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "out/sub")); err != nil {
		t.Fatal(err)
	}

	hfs.ForgetMissingsInDir(ctx, dir, "out")

	if _, err := hfs.Stat(ctx, dir, "out/sub/child"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(out/sub/child) after ForgetMissingsInDir(out) = %v; want fs.ErrNotExist (stale child survived)", err)
	}
}

// TestForgetMissingsInDir_KeepsEmptyDirNode covers the storeNextDir publication
// window: an in-flight child store can publish an empty directory node before
// storing the child below it. Reconcile pruning must not unlink that directory.
func TestForgetMissingsInDir_KeepsEmptyDirNode(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	child := "out/sub/child"
	cd := digest.Digest{Hash: "childhash", SizeBytes: 4}
	err = hfs.Update(ctx, dir, []UpdateEntry{{
		Name:    child,
		Entry:   &merkletree.Entry{Name: child, Data: digest.NewData(nil, cd)},
		Mode:    0o644,
		CmdHash: []byte("cmdhash"),
	}})
	if err != nil {
		t.Fatalf("Update(%q): %v", child, err)
	}
	hfs.directory.deleteForce(ctx, filepath.ToSlash(filepath.Join(dir, child)))
	outSub := filepath.ToSlash(filepath.Join(dir, "out/sub"))
	if e, _, _, ok := hfs.directory.lookup(ctx, outSub); !ok || e == nil || e.getDir() == nil {
		t.Fatalf("precondition: out/sub is not an empty in-memory dir node (ok=%v)", ok)
	}

	hfs.ForgetMissingsInDir(ctx, dir, "out")

	if e, _, _, ok := hfs.directory.lookup(ctx, outSub); !ok || e == nil || e.getDir() == nil {
		t.Errorf("out/sub dir node evicted by ForgetMissingsInDir (ok=%v); want it preserved", ok)
	}
}

// TestForgetMissingsInDir_KeepsGeneratedDescendant is a regression test for a
// review follow-up to the removed-subdir fix: pruning a missing non-generated
// directory must not delete step-generated descendants under it.
//
// An intermediate directory created by storeNextDir is always isChanged=false even
// when it holds a generated (IsChanged) output - e.g. a remote-won is_local=false
// file never materialized on local disk. Force-removing the whole subtree based on
// the directory's own flag would evict that generated output and reintroduce
// spurious "missing outputs". The prune must check the whole subtree and keep it
// when any descendant is changed.
func TestForgetMissingsInDir_KeepsGeneratedDescendant(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	// A generated (IsChanged), remote-won output under out/sub, not on local disk.
	// The intermediate out and out/sub dir nodes are isChanged=false.
	gen := "out/sub/gen.o"
	gd := digest.Digest{Hash: "genhash", SizeBytes: 5}
	err = hfs.Update(ctx, dir, []UpdateEntry{{
		Name:      gen,
		Entry:     &merkletree.Entry{Name: gen, Data: digest.NewData(nil, gd)},
		Mode:      0o644,
		CmdHash:   []byte("cmdhash"),
		IsChanged: true,
	}})
	if err != nil {
		t.Fatalf("Update(%q): %v", gen, err)
	}
	genFull := filepath.ToSlash(filepath.Join(dir, gen))
	if _, _, _, ok := hfs.directory.lookup(ctx, genFull); !ok {
		t.Fatalf("precondition: generated %s not in hashfs", gen)
	}

	// out is gone on local disk (it only held the remote-won output); reconciling
	// it must not evict the generated descendant.
	hfs.ForgetMissingsInDir(ctx, dir, "out")

	if _, _, _, ok := hfs.directory.lookup(ctx, genFull); !ok {
		t.Errorf("generated output %s was pruned by ForgetMissingsInDir; want preserved", gen)
	}
}

// TestForgetMissingsInDir_KeepsUnchangedGeneratedDescendant verifies restat_content
// style outputs: unchanged generated outputs can keep CmdHash while IsChanged is
// false, and still must not be pruned from a missing virtual directory.
func TestForgetMissingsInDir_KeepsUnchangedGeneratedDescendant(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	gen := "out/sub/gen.o"
	gd := digest.Digest{Hash: "genhash", SizeBytes: 5}
	err = hfs.Update(ctx, dir, []UpdateEntry{{
		Name:    gen,
		Entry:   &merkletree.Entry{Name: gen, Data: digest.NewData(nil, gd)},
		Mode:    0o644,
		CmdHash: []byte("cmdhash"),
	}})
	if err != nil {
		t.Fatalf("Update(%q): %v", gen, err)
	}
	genFull := filepath.ToSlash(filepath.Join(dir, gen))
	if _, _, _, ok := hfs.directory.lookup(ctx, genFull); !ok {
		t.Fatalf("precondition: generated %s not in hashfs", gen)
	}

	hfs.ForgetMissingsInDir(ctx, dir, "out")

	if _, _, _, ok := hfs.directory.lookup(ctx, genFull); !ok {
		t.Errorf("unchanged generated output %s was pruned by ForgetMissingsInDir; want preserved", gen)
	}
}

// TestDirectoryDelete_DirReplacedByFile is a regression test for a review
// follow-up to the Mode A guard. When an output path that hashfs has cached as a
// populated directory becomes a regular file on disk, RecordOutputsFromLocal ->
// RetrieveUpdateEntriesFromLocal must be able to invalidate the stale directory
// entry so it can be replaced by the file. The "don't evict populated dirs"
// guard must apply only to parent negative-cache clearing, not to this explicit
// type-change invalidation of the path itself; otherwise the stale directory
// entry survives and hashfs reports out/p as a directory (ReadFile fails).
func TestDirectoryDelete_DirReplacedByFile(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	// hashfs caches "out/p" as a populated directory.
	if err := os.MkdirAll(filepath.Join(dir, "out/p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out/p/child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.ReadDir(ctx, dir, "out/p"); err != nil {
		t.Fatalf("ReadDir(out/p): %v", err)
	}
	if fi, err := hfs.Stat(ctx, dir, "out/p"); err != nil || !fi.IsDir() {
		t.Fatalf("precondition: Stat(out/p) isDir=%v err=%v; want dir", fi.IsDir(), err)
	}

	// On disk the path becomes a regular file (the action now produces a file at
	// out/p instead of a directory).
	if err := os.RemoveAll(filepath.Join(dir, "out/p")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out/p"), []byte("now a file"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Record the local output, as RecordOutputsFromLocal does.
	ents := hfs.RetrieveUpdateEntriesFromLocal(ctx, dir, []string{"out/p"})
	if err := hfs.Update(ctx, dir, ents); err != nil {
		t.Fatalf("Update(out/p): %v", err)
	}

	// hashfs must now see out/p as a regular file, and reading it must succeed.
	fi, err := hfs.Stat(ctx, dir, "out/p")
	if err != nil {
		t.Fatalf("Stat(out/p) after type change: %v", err)
	}
	if fi.IsDir() {
		t.Errorf("Stat(out/p).IsDir() = true; want false (stale directory entry survived the type change)")
	}
	buf, err := hfs.ReadFile(ctx, dir, "out/p")
	if err != nil {
		t.Errorf("ReadFile(out/p) after type change: %v; want success", err)
	} else if string(buf) != "now a file" {
		t.Errorf("ReadFile(out/p) = %q; want %q", buf, "now a file")
	}
}
