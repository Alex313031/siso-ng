// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_OutputDir(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		w, err := os.Create(filepath.Join(dir, "out/siso/siso_explain"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cerr := w.Close()
			if cerr != nil {
				t.Fatal(err)
			}
		}()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		opt.ExplainWriter = w
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	t.Logf("-- setup workspace")
	setupFiles(t, dir, t.Name(), nil)

	t.Logf("-- first build")
	_, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}

	for _, fname := range []string{
		"out/siso/test.app/Frameworks/frameworks/Foo.h",
		"out/siso/test.app/Frameworks/frameworks/Foo2.h",
		"out/siso/obj/frameworks/foo.framework/Foo.h",
		"out/siso/obj/frameworks/foo.framework/Foo2.h",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("stat(%q)=%v; want nil error", fname, err)
		}
	}
	t.Logf("-- confirm no-op")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("ninja confirm no-op error: done=%d local=%d skipped=%d; want done=%d local=%d skipped=%d", stats.Done, stats.Local, stats.Skipped, stats.Total, 0, stats.Total)
		buf, err := os.ReadFile(filepath.Join(dir, "out/siso/siso_explain"))
		t.Logf("siso_explain: %v\n%s", err, buf)
	}

	t.Logf("-- change archive file")
	err = os.Remove(filepath.Join(dir, "framework/Foo2.h"))
	if err != nil {
		t.Fatalf("remove framework/Foo2.h: %v", err)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/build.ninja"))
	if err != nil {
		t.Fatalf("read build.ninja: %v", err)
	}
	buf = bytes.ReplaceAll(buf, []byte(" ../../framework/Foo2.h"), nil)
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), buf, 0644)
	if err != nil {
		t.Fatalf("rewrite build.ninja: %v", err)
	}

	t.Logf("-- second build")
	_, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}

	for _, fname := range []string{
		"out/siso/test.app/Frameworks/frameworks/Foo.h",
		"out/siso/obj/frameworks/foo.framework/Foo.h",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("stat(%q)=%v; want nil error", fname, err)
		}
	}

	for _, fname := range []string{
		"out/siso/test.app/Frameworks/frameworks/Foo2.h",
		"out/siso/obj/frameworks/foo.framework/Foo2.h",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stat(%q)=%v; want %v", fname, err, fs.ErrNotExist)
		}
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("ninja confirm no-op error: done=%d local=%d skipped=%d; want done=%d local=%d skipped=%d", stats.Done, stats.Local, stats.Skipped, stats.Total, 0, stats.Total)
		buf, err := os.ReadFile(filepath.Join(dir, "out/siso/siso_explain"))
		t.Logf("siso_explain: %v\n%s", err, buf)
	}
}
