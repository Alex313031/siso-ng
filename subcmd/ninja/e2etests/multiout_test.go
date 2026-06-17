// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"fmt"
	"path/filepath"
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_MultiOut(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
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
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, refake *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds build.DataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.Client = reapitest.New(ctx, t, refake)
		ds.Cache = ds.Client.CacheStore()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)
	out1Data := []byte("out1")
	out2Data := []byte("out2")
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			out1Digest, err := fakere.Put(ctx, out1Data)
			if err != nil {
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "failed to write out1: %v", err),
				}, nil
			}
			out2Digest, err := fakere.Put(ctx, out2Data)
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
						Digest: out1Digest,
					},
					{
						Path:   "out2",
						Digest: out2Digest,
					},
				},
			}, nil
		},
	}

	stats, err := runNinjaTest(t, fakere)
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
	wantOut1Digest := digest.FromBytes("", out1Data).Digest()
	wantOut2Digest := digest.FromBytes("", out2Data).Digest()
	m := hashfs.StateMap(st)
	e1, ok := m[filepath.ToSlash(filepath.Join(dir, "out/siso/out1"))]
	if !ok {
		t.Errorf("out1 not found: %v", m)
	} else {
		d1 := e1.Digest
		if d1.Hash != wantOut1Digest.Hash || d1.SizeBytes != wantOut1Digest.SizeBytes {
			t.Errorf("out1Digest=%s; want=%s", d1, wantOut1Digest)
		}
	}
	e2, ok := m[filepath.ToSlash(filepath.Join(dir, "out/siso/out2"))]
	if !ok {
		t.Errorf("out2 not found: %v", m)
	} else {
		d2 := e2.Digest
		if d2.Hash != wantOut2Digest.Hash || d2.SizeBytes != wantOut2Digest.SizeBytes {
			t.Errorf("out2Digest=%s; want=%s", d2, wantOut2Digest)
		}
	}
}
