// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"fmt"
	"path"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// test precomputed tree for sysroot/frameworks.
func TestBuild_ObjcxxFrameworks(t *testing.T) {
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
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			var d *rpb.Digest
			for _, fname := range []string{
				"../../ios/foo.mm",
				"sdk/xcode_links/iPhoneSimulator.sdk/SDKSettings.json",
				"sdk/xcode_links/iPhoneSimulator.sdk/Developer/Library/Frameworks/Foo.frameworks/Headers/Foo.h",
				"sdk/xcode_links/iPhoneSimulator.platform/Developer/Library/Frameworks/Bar.frameworks/Headers/Bar.h",
			} {
				fn, err := tree.LookupFileNode(ctx, path.Join("out/siso", fname))
				if err != nil {
					t.Logf("missing %s in input", fname)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "%s: File not found: %v", fname, err),
					}, nil
				}
				t.Logf("input %s is ok", fname)
				if fname == "../../ios/foo.mm" {
					d = fn.Digest
				}
			}
			dd, err := fakere.Put(ctx, []byte("obj/ios/foo.o: ../../ios/foo.mm\n"))
			if err != nil {
				msg := fmt.Sprintf("failed to write obj/ios/foo.o.d: %v", err)
				t.Log(msg)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte(msg),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "obj/ios/foo.o",
						Digest: d,
					},
					{
						Path:   "obj/ios/foo.o.d",
						Digest: dd,
					},
				},
			}, nil
		},
	}
	stats, err := ninja(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total || stats.Remote != 1 {
		t.Errorf("done=%d remote=%d total=%d; want done=total, remote=1: %#v", stats.Done, stats.Remote, stats.Total, stats)
	}
}
