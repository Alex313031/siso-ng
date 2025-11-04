// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TODO(b/267409605): At minimum have test parity with
// https://github.com/ninja-build/ninja/blob/d4017a2b1ea642f12dabe05ec99b2a16c93e99aa/src/deps_log_test.cc

func TestReadWriteDepsLog(t *testing.T) {
	ctx := t.Context()
	fname := filepath.Join(t.TempDir(), "mydepslog")
	t1 := time.Unix(1, 0)
	t2 := time.Unix(2, 0)

	dl1, err := NewDepsLog(ctx, fname)
	if err != nil {
		t.Errorf("NewDepsLog(ctx, %s)=_, %v; want nil error", fname, err)
	}
	r, err := dl1.Record(ctx, "out.o", t1, []string{"foo.h", "bar.h"})
	if err != nil || !r {
		t.Errorf(`dl1.Record(ctx, "out.o", %v, []string{"foo.h", "bar.h"})=%t, %v; want true, nil error`, t1, r, err)
	}
	r, err = dl1.Record(ctx, "out2.o", t2, []string{"foo.h", "bar2.h"})
	if err != nil || !r {
		t.Errorf(`dl1.Record(ctx, "out2.o", %v, []string{"foo.h", "bar2.h"})=%t, %v; want true, nil error`, t2, r, err)
	}
	// Get will not see recorded entry in the same session.
	var want []string
	deps, mtime, err := dl1.RetrievePaths(ctx, "out.o")
	if err == nil {
		t.Errorf(`d1.RetrievePaths(ctx, "out.o")=_, _, %v; want _, _, error`, err)
	}
	if diff := cmp.Diff(deps, want); diff != "" {
		t.Errorf(`d1.RetrievePaths(ctx, "out.o")=%v, _, _ mismatch (-got +want):\n%s`, deps, diff)
	}
	if mtime.Equal(t1) {
		t.Errorf(`d1.RetrievePaths(ctx, "out.o")=_, %v, _; want _, %v, _`, mtime, t1)
	}

	err = dl1.Close()
	if err != nil {
		t.Errorf("dl1.Close=%v; want nil error", err)
	}
	t.Logf("second deplog")
	dl2, err := NewDepsLog(ctx, fname)
	if err != nil {
		t.Errorf(`NewDepsLog(ctx, %s)=_, %v; want nil error`, fname, err)
	}
	defer func() {
		err := dl2.Close()
		if err != nil {
			t.Errorf("dl2.Close=%v; want nil error", err)
		}
	}()

	deps, mtime, err = dl2.RetrievePaths(ctx, "out.o")
	if err != nil {
		t.Errorf(`dl2.RetrievePaths(ctx, "out.o")=_, _, %v; want _, _, nil error`, err)
	}
	want = []string{"foo.h", "bar.h"}
	if diff := cmp.Diff(deps, want); diff != "" {
		t.Errorf(`dl2.RetrievePaths(ctx, "out.o")=%v, _, _ mismatch (-got +want):\n%s`, deps, diff)
	}
	if !mtime.Equal(t1) {
		t.Errorf(`dl2.RetrivePaths(ctx, "out.o")=_, %v, _; want _, %v, _`, mtime, t1)
	}
	want = []string{"foo.h", "bar2.h"}
	deps, mtime, err = dl2.RetrievePaths(ctx, "out2.o")
	if err != nil {
		t.Errorf(`dl2.RetrievePaths(ctx, "out2.o")=_, _, %v; want _, _, nil error`, err)
	}
	if diff := cmp.Diff(deps, want); diff != "" {
		t.Errorf(`dl2.RetrievePaths(ctx, "out2.o")=%v, _, _ mismatch (-got +want):\n%s`, deps, diff)
	}
	if !mtime.Equal(t2) {
		t.Errorf(`dl2.RetrievePaths(ctx, "out2.o")=_, %v, _; want _, %v, _`, mtime, t2)
	}
}

func TestRecompact(t *testing.T) {
	ctx := t.Context()
	fname := filepath.Join(t.TempDir(), "mydepslog")
	t1 := time.Unix(1, 0)
	t2 := time.Unix(2, 0)
	t3 := time.Unix(3, 0)

	var fileSize1 int64
	t.Logf("Write some deps to the file and grab its size.")
	{
		dl, err := NewDepsLog(ctx, fname)
		if err != nil {
			t.Fatalf("NewDepsLog(ctx, %s)=_, %v; want nil error", fname, err)
		}
		r, err := dl.Record(ctx, "out.o", t1, []string{"foo.h", "bar.h"})
		if err != nil || !r {
			t.Fatalf(`dl.Record(ctx, "out.o", %v, {"foo.h", "bar.h")=%t, %v; want true, nil error`, t1, r, err)
		}
		r, err = dl.Record(ctx, "other_out.o", t2, []string{"foo.h", "baz.h"})
		if err != nil || !r {
			t.Fatalf(`dl.Record(ctx, "other_out.o", %v, {"foo.h", "baz.h")=%t, %v; want true, nil error`, t2, r, err)
		}
		err = dl.Close()
		if err != nil {
			t.Fatalf("dl.Close()=%v; want nil err", err)
		}

		fi, err := os.Stat(fname)
		if err != nil {
			t.Fatalf("Stat(%q)=_, %v; want nil err", fname, err)
		}
		fileSize1 = fi.Size()
		t.Logf("depslog size=%d", fileSize1)
	}

	var fileSize2 int64
	t.Logf("Now reload the file, and add slightly different deps.")
	{
		dl, err := NewDepsLog(ctx, fname)
		if err != nil {
			t.Fatalf("NewDepsLog(ctx, %s)=_, %v; want nil error", fname, err)
		}
		r, err := dl.Record(ctx, "out.o", t3, []string{"foo.h"})
		if err != nil || !r {
			t.Fatalf(`dl.Record(ctx, "out.o", %v, {"foo.h"})=%t, %v; want true, nil error`, t1, r, err)
		}
		err = dl.Close()
		if err != nil {
			t.Fatalf("dl.Close()=%v; want nil err", err)
		}

		fi, err := os.Stat(fname)
		if err != nil {
			t.Fatalf("Stat(%q)=_, %v; want nil err", fname, err)
		}
		fileSize2 = fi.Size()
		t.Logf("depslog size=%d", fileSize2)
	}
	if fileSize2 <= fileSize1 {
		t.Errorf("deps log size %d must be bigger than %d", fileSize2, fileSize1)
	}

	var fileSize3 int64
	t.Logf("Now reload the file, verify the new deps have replaced the old, then recompact.")
	{
		dl, err := NewDepsLog(ctx, fname)
		if err != nil {
			t.Fatalf("NewDepsLog(ctx, %s)=_, %v; want nil error", fname, err)
		}
		deps, ts, err := dl.RetrievePaths(ctx, "out.o")
		want := []string{"foo.h"}
		if !cmp.Equal(deps, want) || !ts.Equal(t3) || err != nil {
			t.Errorf(`dl.RetrievePaths(ctx, "out.o")=%v, %v, %v; want %v, %v, %v`, deps, ts, err, want, t3, nil)
		}

		deps, ts, err = dl.RetrievePaths(ctx, "other_out.o")
		want = []string{"foo.h", "baz.h"}
		if !cmp.Equal(deps, want) || !ts.Equal(t2) || err != nil {
			t.Errorf(`d1.RetrievePaths(ctx, "other_out.o")=%v, %v, %v; want %v, %v, %v`, deps, ts, err, want, t2, nil)
		}

		err = dl.Recompact(ctx)
		if err != nil {
			t.Errorf("dl.Recompact(ctx)=%v; want nil err", err)
		}

		t.Logf("The in-memory deps graph should still be valid after recompaction.")
		deps, ts, err = dl.RetrievePaths(ctx, "out.o")
		want = []string{"foo.h"}
		if !cmp.Equal(deps, want) || !ts.Equal(t3) || err != nil {
			t.Errorf(`dl.RetrievePaths(ctx, "out.o")=%v, %v, %v; want %v, %v, %v`, deps, ts, err, want, t3, nil)
		}
		deps, ts, err = dl.RetrievePaths(ctx, "other_out.o")
		want = []string{"foo.h", "baz.h"}
		if !cmp.Equal(deps, want) || !ts.Equal(t2) || err != nil {
			t.Errorf(`d1.RetrievePaths("other_out.o")=%v, %v, %v; want %v, %v, %v`, deps, ts, err, want, t2, nil)
		}

		err = dl.Close()
		if err != nil {
			t.Fatalf("dl.Close()=%v; want nil err", err)
		}

		t.Logf("The file should have shrunk a bit for the smaller deps.")
		fi, err := os.Stat(fname)
		if err != nil {
			t.Fatalf("Stat(%q)=_, %v; want nil err", fname, err)
		}
		fileSize3 = fi.Size()
		t.Logf("depslog size=%d", fileSize3)
	}
	if fileSize3 >= fileSize2 {
		t.Errorf("deps log size %d must be smaller than %d", fileSize3, fileSize2)
	}
}

func TestDepsLog_broken(t *testing.T) {
	ctx := t.Context()
	fname := filepath.Join(t.TempDir(), "mydepslog")
	t1 := time.Unix(1, 0)
	t2 := time.Unix(2, 0)

	t.Logf("-- create deps log")
	dl1, err := NewDepsLog(ctx, fname)
	if err != nil {
		t.Fatalf("NewDepsLog(ctx, %q)=_, %v; want nil error", fname, err)
	}
	r, err := dl1.Record(ctx, "out.o", t1, []string{"foo.h", "bar.h"})
	if err != nil || !r {
		t.Fatalf(`dl1.Record(ctx, "out.o", %v, []string{"foo.h", "bar.h"})=%t, %v; want true, nil error`, t1, r, err)
	}
	r, err = dl1.Record(ctx, "out2.o", t2, []string{"foo.h", "bar2.h"})
	if err != nil || !r {
		t.Fatalf(`dl1.Record(ctx, "out2.o", %v, []string{"foo.h", "bar2.h"})=%t, %v; want true, nil error`, t2, r, err)
	}
	err = dl1.Close()
	if err != nil {
		t.Fatalf("dl1.Close=%v; want nil error", err)
	}
	t.Logf("-- truncate deps log")
	fi, err := os.Stat(fname)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 144 {
		t.Errorf("deps log size=%d; want 144", fi.Size())
	}
	err = os.Truncate(fname, 144-5*8-1)
	if err != nil {
		t.Errorf("truncate(%d)=%v; want nil error", 144-5*8-1, err)
	}
	t.Logf("-- load deps log again. expect recompact")
	dl2, err := NewDepsLog(ctx, fname)
	if err != nil {
		t.Errorf("NewDepsLog(ctx, %q)=_, %v; want nil error", fname, err)
	}
	if !dl2.needsRecompact {
		t.Errorf("dl2.needsRecompact=%t; want true", dl2.needsRecompact)
	}
	err = dl2.Recompact(ctx)
	if err != nil {
		t.Errorf("dl2.Recompact=%v; want nil error", err)
	}
	err = dl2.Close()
	if err != nil {
		t.Errorf("dl2.Close=%v; want nil error", err)
	}
	t.Logf("-- load deps log again, no recompact")
	dl3, err := NewDepsLog(ctx, fname)
	if err != nil {
		t.Errorf("NewDepsLog(ctx, %q)=_, %v; want nil error", fname, err)
	}
	if dl3.needsRecompact {
		t.Errorf("dl3.needsRecompact=%t; want false", dl3.needsRecompact)
	}
	err = dl3.Close()
	if err != nil {
		t.Errorf("dl3.Close=%v; want nil error", err)
	}
}
