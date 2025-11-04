// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_OutputLocal(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, outputLocal bool, fakere *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds dataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.client = reapitest.New(ctx, t, fakere)
		ds.cache = ds.client.CacheStore()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return outputLocal },
			DataSource:  ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.client
		opt.OutputLocal = func(context.Context, string) bool { return outputLocal }
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			fn, err := tree.LookupFileNode(ctx, "input")
			if err != nil {
				t.Logf("missing input: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "../../input: File not found: %v", err),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "out0",
						Digest: fn.Digest,
					},
					{
						Path:   "out1",
						Digest: fn.Digest,
					},
				},
			}, nil
		},
	}

	checkFile := func(dir, name string, data []byte) error {
		buf, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if diff := cmp.Diff(data, buf); diff != "" {
			return fmt.Errorf("unexpected content for %q: -want +got:\n%s", name, diff)
		}
		return nil
	}

	t.Logf("-- first build. output_local=true")
	stats, err := ninja(t, true, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 3 || stats.Remote != 1 || stats.Local != 2 {
		t.Errorf("done=%d total=%d remote=%d local=%d; want done=total=3 remote=1 local=2: %#v", stats.Done, stats.Total, stats.Remote, stats.Local, stats)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "input"))
	if err != nil {
		t.Fatal(err)
	}
	data := buf
	for _, name := range []string{"out0", "out1", "out2", "out3"} {
		err = checkFile(dir, filepath.Join("out/siso", name), data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	t.Logf("-- confirm no op")
	stats, err = ninja(t, true, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 3 || stats.Skipped != 3 {
		t.Errorf("done=%d total=%d skipped=%d; want done=total=skipped=3; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	data = append(buf, []byte(" modified")...)
	modifyFile(t, dir, "input", func([]byte) []byte { return data })

	err = checkFile(dir, "input", data)
	if err != nil {
		t.Error(err)
	}

	t.Logf("-- second build. output_local=false")
	stats, err = ninja(t, false, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 3 || stats.Remote != 1 || stats.Local != 2 {
		t.Errorf("done=%d total=%d remote=%d local=%d; want done=total=3 remote=1 local=2: %#v", stats.Done, stats.Total, stats.Remote, stats.Local, stats)
	}

	for _, name := range []string{"out0", "out1", "out2", "out3"} {
		err = checkFile(dir, filepath.Join("out/siso", name), data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	t.Logf("-- confirm no op")
	stats, err = ninja(t, false, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 3 || stats.Skipped != 3 {
		t.Errorf("done=%d total=%d skipped=%d; want done=total=skipped=3; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
}
