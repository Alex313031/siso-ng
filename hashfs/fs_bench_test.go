// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs/osfs"
	"go.chromium.org/build/siso/path"
)

// BenchmarkFlushBuf measures the cost of flushing a buf-backed entry (the
// path taken by ctx.actions.write outputs and copies of small in-memory
// files) to disk. The "create" sub-benchmarks write a fresh file; the
// "overwrite" ones replace an existing file, which is the case where atomic
// (tmp+rename) vs in-place (O_TRUNC) write differ. Content varies per
// iteration so the digest never matches and the real write path is exercised.
func BenchmarkFlushBuf(b *testing.B) {
	ctx := b.Context()
	ofs := osfs.New(ctx, "bench", osfs.Option{})
	for _, size := range []int{16, 4096} {
		base := make([]byte, size)
		for i := range base {
			base[i] = 'x'
		}
		newEntry := func(fname string, buf []byte, mtime time.Time) *entry {
			data := blob.FromBytes(digest.SHA256, fname, buf)
			lready := make(chan bool, 1)
			lready <- true
			return &entry{
				lready: lready,
				size:   int64(len(buf)),
				mode:   0644,
				src:    data,
				d:      data.Digest(),
				buf:    buf,
				mtime:  mtime,
			}
		}
		b.Run(fmt.Sprintf("overwrite/%dB", size), func(b *testing.B) {
			dir := b.TempDir()
			fname := filepath.Join(dir, "out.bin")
			if err := os.WriteFile(fname, base, 0644); err != nil {
				b.Fatal(err)
			}
			mtime := time.Now()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				buf := make([]byte, size)
				copy(buf, base)
				buf[0] = byte(i)
				mtime = mtime.Add(time.Second)
				i++
				if err := newEntry(fname, buf, mtime).flush(ctx, fname, ofs, time.Minute); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("create/%dB", size), func(b *testing.B) {
			dir := b.TempDir()
			fname := filepath.Join(dir, "out.bin")
			mtime := time.Now()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				buf := make([]byte, size)
				copy(buf, base)
				buf[0] = byte(i)
				mtime = mtime.Add(time.Second)
				i++
				if err := newEntry(fname, buf, mtime).flush(ctx, fname, ofs, time.Minute); err != nil {
					b.Fatal(err)
				}
				// Remove so the next iteration is a fresh create. This cost is
				// constant across baseline/patched runs, so benchstat's delta
				// still isolates the added rename.
				if err := os.Remove(fname); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDirectoryLookup(b *testing.B) {
	ctx := b.Context()
	root := &directory{isRoot: true}
	dir := b.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		b.Fatal(err)
	}
	fname := path.New(filepath.Join(dir, "gen"))
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _, _, ok := root.lookup(ctx, fname)
			if ok {
				b.Fatalf("lookup(ctx, %q)=_, _, %t; want false", fname, ok)
			}
		}
	})
	e := &entry{err: fs.ErrNotExist}
	root.store(ctx, fname, e)
	b.Run("ok", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _, _, ok := root.lookup(ctx, fname)
			if !ok {
				b.Fatalf("lookup(ctx, %q)=_, _, %t; want true", fname, ok)
			}
		}
	})
}

// test to make sure keep allocations under
// allocations that was measured by the above benchmark.
// fs_test.go is external test, but this is internal test.
func TestDirectoryLookup(t *testing.T) {
	ctx := t.Context()
	root := &directory{isRoot: true}
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	fname := path.New(filepath.Join(dir, "gen"))

	t.Run("miss", func(t *testing.T) {
		num := 1000
		if log.V(1) {
			num = 1
		}
		avg := testing.AllocsPerRun(num, func() {
			_, _, _, ok := root.lookup(ctx, fname)
			if ok {
				t.Fatalf("lookup(ctx, %q)=_, _, %t; want false", fname, ok)
			}
		})
		if avg != 0 {
			t.Errorf("alloc=%f; want 0", avg)
		}
	})

	e := &entry{err: fs.ErrNotExist}
	root.store(ctx, fname, e)
	t.Run("ok", func(t *testing.T) {
		num := 1000
		if log.V(1) {
			num = 1
		}
		avg := testing.AllocsPerRun(num, func() {
			_, _, _, ok := root.lookup(ctx, fname)
			if !ok {
				t.Fatalf("lookup(ctx, %q)=_, _, %t; want true", fname, ok)
			}
		})
		if avg != 0 {
			t.Errorf("alloc=%f; want 0", avg)
		}
	})
}

// BenchmarkCopyImpl compares ways to materialize a "copy" action output that
// has real file content (the FileSource path, i.e. flushRegularFile's default
// case -> flushWrite), against the external implementations a GN copy could
// use instead:
//
//   - siso:          Siso's in-process hashfs flush (tmp+rename, or COW clone
//     on platforms that support it). No subprocess.
//   - shell_hardlink: the literal GN copy_command, where `ln -f` succeeds on
//     the same filesystem (the common fast path; before==after
//     since `ln` short-circuits before the patched fallback).
//   - shell_cp_before: the pre-CL fallback, run in isolation (non-atomic):
//     rm -rf dst && cp -af src dst   (commit e0a287b3, before)
//   - shell_cp_after:  the post-CL fallback, run in isolation (atomic):
//     cp -af src dst.tmp && mv -f dst.tmp dst   (e0a287b3, after)
//   - copy_atomic_py:  tools/typescript/copy_atomic.py (commit 47aac0539), a
//     full content copy via action_helpers.atomic_output.
//
// The fallback variants are benchmarked on their own because that is the only
// part the e0a287b3 patch changed and the only place atomicity differs; in the
// same-filesystem common case `ln -f` wins and the fallback never runs. dst is
// removed each iteration so every variant performs a real create-copy.
func BenchmarkCopyImpl(b *testing.B) {
	ctx := b.Context()
	ofs := osfs.New(ctx, "bench", osfs.Option{})

	// Write the copy_atomic.py reproduction and its action_helpers dependency
	// once, so the benchmark is self-contained (no Chromium checkout needed).
	scriptDir := b.TempDir()
	copyAtomicPath := filepath.Join(scriptDir, "copy_atomic.py")
	if err := os.WriteFile(copyAtomicPath, []byte(copyAtomicPy), 0755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "action_helpers.py"), []byte(actionHelpersPy), 0644); err != nil {
		b.Fatal(err)
	}

	sizes := []struct {
		name string
		n    int
	}{
		{"4KiB", 4 << 10},
		{"1MiB", 1 << 20},
	}
	for _, size := range sizes {
		content := make([]byte, size.n)
		for i := range content {
			content[i] = byte(i)
		}
		data := blob.FromBytes(digest.SHA256, "", content)

		// run sets up a src file and a dst path on one filesystem, then drives
		// fn(src, dst) once per iteration after removing any prior dst.
		run := func(b *testing.B, fn func(src, dst string) error) {
			dir := b.TempDir()
			src := filepath.Join(dir, "src.bin")
			dst := filepath.Join(dir, "out", "dst.bin")
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(src, content, 0644); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(size.n))
			b.ReportAllocs()
			for b.Loop() {
				os.Remove(dst)
				if err := fn(src, dst); err != nil {
					b.Fatal(err)
				}
			}
		}
		sh := func(src, dst, script string) error {
			out, err := exec.Command("sh", "-c", script, "sh", src, dst).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%v: %s", err, out)
			}
			return nil
		}

		b.Run(fmt.Sprintf("siso/%s", size.name), func(b *testing.B) {
			mtime := time.Now()
			run(b, func(src, dst string) error {
				lready := make(chan bool, 1)
				lready <- true
				e := &entry{
					lready: lready,
					size:   int64(size.n),
					mode:   0644,
					src:    ofs.FileSource(src, int64(size.n)),
					d:      data.Digest(),
					mtime:  mtime,
				}
				return e.flush(ctx, dst, ofs, time.Minute)
			})
		})
		b.Run(fmt.Sprintf("shell_hardlink/%s", size.name), func(b *testing.B) {
			run(b, func(src, dst string) error {
				return sh(src, dst, `ln -f "$1" "$2" 2>/dev/null || (rm -rf "$2" && cp -af "$1" "$2")`)
			})
		})
		b.Run(fmt.Sprintf("shell_cp_before/%s", size.name), func(b *testing.B) {
			run(b, func(src, dst string) error {
				return sh(src, dst, `rm -rf "$2" && cp -af "$1" "$2"`)
			})
		})
		b.Run(fmt.Sprintf("shell_cp_after/%s", size.name), func(b *testing.B) {
			run(b, func(src, dst string) error {
				return sh(src, dst, `rm -rf "$2.tmp" && cp -af "$1" "$2.tmp" && mv -f "$2.tmp" "$2"`)
			})
		})
		b.Run(fmt.Sprintf("copy_atomic_py/%s", size.name), func(b *testing.B) {
			run(b, func(src, dst string) error {
				out, err := exec.Command("python3", copyAtomicPath, src, dst).CombinedOutput()
				if err != nil {
					return fmt.Errorf("%v: %s", err, out)
				}
				return nil
			})
		})
	}
}

// copyAtomicPy is tools/typescript/copy_atomic.py from Chromium commit
// 47aac0539566a83c8f5ac442fc162bf23e5a928a, verbatim except for the sys.path
// line, which is pointed at this script's own directory so it imports the
// co-located action_helpers reproduction below.
const copyAtomicPy = `#!/usr/bin/env python3
import os
import shutil
import sys

# Import the co-located action_helpers reproduction (upstream appends //build).
sys.path.append(os.path.dirname(__file__))
import action_helpers


def main():
  if len(sys.argv) != 3:
    print(f"Usage: {sys.argv[0]} <src> <dst>", file=sys.stderr)
    return 1

  src = sys.argv[1]
  dst = sys.argv[2]

  dst = os.path.normpath(dst)
  with action_helpers.atomic_output(dst) as f:
    with open(src, 'rb') as fsrc:
      shutil.copyfileobj(fsrc, f)
    shutil.copymode(src, f.name)
  return 0


if __name__ == '__main__':
  sys.exit(main())
`

// actionHelpersPy reproduces build/action_helpers.py's atomic_output (the only
// part copy_atomic.py uses), verbatim from the same checkout.
const actionHelpersPy = `import contextlib
import filecmp
import os
import shutil
import tempfile


@contextlib.contextmanager
def atomic_output(path, mode='w+b', encoding=None, only_if_changed=True):
  # Create in same directory to ensure same filesystem when moving.
  dirname = os.path.dirname(path) or '.'
  os.makedirs(dirname, exist_ok=True)
  if encoding is not None and mode == 'w+b':
    mode = 'w+'
  with tempfile.NamedTemporaryFile(mode,
                                   encoding=encoding,
                                   prefix=".tempfile.",
                                   suffix="." + os.path.basename(path),
                                   dir=dirname,
                                   delete=False) as f:
    try:
      yield f

      # File should be closed before comparison/move.
      f.close()
      if not (only_if_changed and os.path.exists(path)
              and filecmp.cmp(f.name, path)):
        shutil.move(f.name, path)
    finally:
      f.close()
      if os.path.exists(f.name):
        os.unlink(f.name)
`
