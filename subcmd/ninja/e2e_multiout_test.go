// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"fmt"
	"path/filepath"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_MultiOut(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
	defer cleanup()

	b, err := build.New(ctx, graph, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	err = b.Build(ctx, "build", "all")
	if err != nil {
		t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
	}

	stats := b.Stats()
	t.Logf("err %v; %#v", err, stats)
	if stats.Done != stats.Total {
		t.Errorf("stats.Done=%d Total=%d", stats.Done, stats.Total)
	}
}

// Test step that outputs multiple targets correctly generates the outputs.
func TestBuild_MultiOut_Remote(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, refake *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds dataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.client = reapitest.New(ctx, t, refake)
		ds.cache = ds.client.CacheStore()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.client
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)
	var out1, out2 *rpb.Digest
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			var err error
			out1, err = fakere.Put(ctx, []byte("out1"))
			if err != nil {
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "failed to write out1: %v", err),
				}, nil
			}
			out2, err = fakere.Put(ctx, []byte("out2"))
			if err != nil {
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "failed to write out2: %v", err),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "out1",
						Digest: out1,
					},
					{
						Path:   "out2",
						Digest: out2,
					},
				},
			}, nil
		},
	}

	stats, err := ninja(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total {
		t.Errorf("stats.Done=%d Total=%d", stats.Done, stats.Total)
	}
	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
	if err != nil {
		t.Errorf("hashfs.Load=%v; want nil err", err)
	}
	m := hashfs.StateMap(st)
	e1, ok := m[filepath.ToSlash(filepath.Join(dir, "out/siso/out1"))]
	if !ok {
		t.Errorf("out1 not found: %v", m)
	} else {
		d1 := e1.Digest
		if d1.Hash != out1.Hash || d1.SizeBytes != out1.SizeBytes {
			t.Errorf("out1=%s; want=%s", d1, out1)
		}
	}
	e2, ok := m[filepath.ToSlash(filepath.Join(dir, "out/siso/out2"))]
	if !ok {
		t.Errorf("out2 not found: %v", m)
	} else {
		d2 := e2.Digest
		if d2.Hash != out2.Hash || d2.SizeBytes != out2.SizeBytes {
			t.Errorf("out2=%s; want=%s", d2, out2)
		}
	}
}
