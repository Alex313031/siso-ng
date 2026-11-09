// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// mkTreeForDigest materializes a synthetic generated-output tree under root/gen
// with nFiles regular files across a few subdirectories.
func mkTreeForDigest(t testing.TB, root string, nFiles int) {
	t.Helper()
	const dir = "gen"
	const subdirs = 8
	for i := range nFiles {
		rel := filepath.Join(dir, fmt.Sprintf("sub%d", i%subdirs), fmt.Sprintf("f%d.dat", i))
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, fmt.Appendf(nil, "content-%d-payload", i), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func newDigestHashFS(t testing.TB) (*HashFS, string) {
	t.Helper()
	ctx := t.Context()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hfs.Close(ctx) })
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return hfs, root
}

// TestExpandDirInputs_IncludesEmptySubdir is a regression test: a directory
// input containing an empty subdirectory must expand to include that subdir, so
// a remote action's input tree matches local disk. The output side preserves
// empty dirs (dirOutputTree); the input side must too.
func TestExpandDirInputs_IncludesEmptySubdir(t *testing.T) {
	hfs, root := newDigestHashFS(t)
	if err := os.MkdirAll(filepath.Join(root, "d", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := hfs.expandDirInputs(t.Context(), root, []string{"d/"})
	if !slices.Contains(got, "d/empty") {
		t.Errorf("expandDirInputs(d/) = %v; want it to include the empty subdir d/empty", got)
	}
}

// TestExpandDirInputs_AmortizedAcrossActions proves a directory input consumed
// by many actions in one build is walked once, not once per consuming action.
func TestExpandDirInputs_AmortizedAcrossActions(t *testing.T) {
	ctx := t.Context()
	hfs, root := newDigestHashFS(t)
	const nFiles = 30
	mkTreeForDigest(t, root, nFiles)

	const actions = 5
	expandDirInputsWalks.Store(0)
	for range actions {
		got := hfs.expandDirInputs(ctx, root, []string{"gen/"})
		if len(got) != nFiles {
			t.Fatalf("expandDirInputs returned %d files; want %d", len(got), nFiles)
		}
	}
	if walks := expandDirInputsWalks.Load(); walks != 1 {
		t.Errorf("expandDirInputs walked the tree %d times for %d consumers; want 1 (not amortized across actions)", walks, actions)
	}
}

// TestExpandDirInputs_InvalidatedOnUpdate verifies that recording an output
// under a cached directory input drops the cached expansion, so the next
// consumer re-walks and sees the new file (a producer can finish after an
// earlier consumer expanded the same directory).
func TestExpandDirInputs_InvalidatedOnUpdate(t *testing.T) {
	ctx := t.Context()
	hfs, root := newDigestHashFS(t)
	mkTreeForDigest(t, root, 5)

	expandDirInputsWalks.Store(0)
	hfs.expandDirInputs(ctx, root, []string{"gen/"}) // walk 1, caches
	hfs.expandDirInputs(ctx, root, []string{"gen/"}) // cache hit, no walk
	if walks := expandDirInputsWalks.Load(); walks != 1 {
		t.Fatalf("before mutation: walks=%d; want 1 (second expand should hit cache)", walks)
	}

	data := blob.FromBytes(digest.SHA256, "newf", []byte("NEW"))
	if err := hfs.Update(ctx, root, []UpdateEntry{{
		Name:    "gen/sub0/newf",
		Entry:   &merkletree.Entry{Data: data},
		Mode:    0644,
		ModTime: time.Unix(1000, 0),
	}}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	hfs.expandDirInputs(ctx, root, []string{"gen/"}) // must re-walk
	if walks := expandDirInputsWalks.Load(); walks != 2 {
		t.Errorf("Update under a cached dir input did not invalidate it: walks=%d; want 2 (stale expansion)", walks)
	}
}

// TestExpandDirInputs_InvalidatedOnRemoveAll verifies removal under a cached
// directory input drops the cached expansion, so the next consumer re-walks
// and the removed file no longer appears.
func TestExpandDirInputs_InvalidatedOnRemoveAll(t *testing.T) {
	ctx := t.Context()
	hfs, root := newDigestHashFS(t)
	mkTreeForDigest(t, root, 6)

	first := hfs.expandDirInputs(ctx, root, []string{"gen/"})
	victim := first[0]
	if !slices.Contains(first, victim) {
		t.Fatalf("setup: %q not in expansion %v", victim, first)
	}

	if err := hfs.RemoveAll(ctx, root, path.Path(victim)); err != nil {
		t.Fatalf("RemoveAll(%q): %v", victim, err)
	}

	second := hfs.expandDirInputs(ctx, root, []string{"gen/"})
	if slices.Contains(second, victim) {
		t.Errorf("removed file %q still in expansion %v (stale cache after RemoveAll)", victim, second)
	}
}

// TestExpandDirInputs_InvalidatedByAllMutations covers the remaining
// structural-mutation hooks (Remove, Forget, ForgetMissingsInDir) that must
// drop a cached directory-input expansion (Update and RemoveAll are above).
func TestExpandDirInputs_InvalidatedByAllMutations(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, hfs *HashFS, root, victim string)
	}{
		{"Remove", func(t *testing.T, hfs *HashFS, root, victim string) {
			if err := hfs.Remove(ctx, root, path.Path(victim)); err != nil {
				t.Fatal(err)
			}
		}},
		{"Forget", func(t *testing.T, hfs *HashFS, root, victim string) {
			hfs.Forget(ctx, root, []path.Path{path.Path(victim)})
		}},
		{"ForgetMissingsInDir", func(t *testing.T, hfs *HashFS, root, victim string) {
			hfs.ForgetMissingsInDir(ctx, root, "gen")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hfs, root := newDigestHashFS(t)
			mkTreeForDigest(t, root, 8)

			first := hfs.expandDirInputs(ctx, root, []string{"gen/"})
			if len(first) == 0 {
				t.Fatal("empty expansion in setup")
			}
			// Warm the cache, then count only post-warm walks.
			expandDirInputsWalks.Store(0)
			hfs.expandDirInputs(ctx, root, []string{"gen/"})
			if w := expandDirInputsWalks.Load(); w != 0 {
				t.Fatalf("%s: cache not warm, pre-mutation expand re-walked (walks=%d)", tc.name, w)
			}

			tc.mutate(t, hfs, root, first[0])

			hfs.expandDirInputs(ctx, root, []string{"gen/"})
			if w := expandDirInputsWalks.Load(); w != 1 {
				t.Errorf("%s did not invalidate the cached dir-input expansion: post-mutation walks=%d; want 1", tc.name, w)
			}
		})
	}
}

// BenchmarkExpandDirInputs measures the per-call cost of expanding a directory
// input on a warm cache (the per-consumer fan-out cost), reporting walks/op.
func BenchmarkExpandDirInputs(b *testing.B) {
	ctx := b.Context()
	hfs, root := newDigestHashFS(b)
	mkTreeForDigest(b, root, 1000)
	hfs.expandDirInputs(ctx, root, []string{"gen/"}) // warm

	expandDirInputsWalks.Store(0)
	b.ReportAllocs()
	for b.Loop() {
		hfs.expandDirInputs(ctx, root, []string{"gen/"})
	}
	b.ReportMetric(float64(expandDirInputsWalks.Load())/float64(b.N), "walks/op")
}
