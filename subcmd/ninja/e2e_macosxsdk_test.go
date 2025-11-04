// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"fmt"
	"path"
	"runtime"
	"strings"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// test precomputed tree for sysroot and frameworks dir inside sysroot.
func TestBuild_MacOSXSDK(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip: no symlink support on windows")
		return
	}
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
				"../../mac/foo.mm",
				"../../build/mac_files/SDKs/MacOSX.sdk/SDKSettings.json",
				"../../build/mac_files/SDKs/MacOSX.sdk/System/Library/Frameworks/Foo.framework/Versions/A/Headers/Foo.h",
			} {
				fn, err := tree.LookupFileNode(ctx, path.Join("out/siso", fname))
				if err != nil {
					t.Logf("missing file %s in input", fname)
					var buf strings.Builder
					err := tree.Dump(ctx, &buf)
					if err != nil {
						t.Errorf("failed to dump: %v", err)
					}
					t.Logf("input tree:\n%s", buf.String())

					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "%s: File not found: %v", fname, err),
					}, nil
				}
				t.Logf("input %s is ok", fname)
				if fname == "../../mac/foo.mm" {
					d = fn.Digest
				}
			}
			for _, sname := range []string{
				"../../build/mac_files/SDKs/MacOSX14.0.sdk",
				"../../build/mac_files/SDKs/MacOSX.sdk/System/Library/PrivateFrameworks/Foo.framework",
				"../../build/mac_files/SDKs/MacOSX.sdk/System/Library/Frameworks/Foo.framework/Headers",
				"../../build/mac_files/SDKs/MacOSX.sdk/System/Library/Frameworks/Foo.framework/Versions/Current",
			} {
				sn, err := tree.LookupSymlinkNode(ctx, path.Join("out/siso", sname))
				if err != nil {
					t.Logf("missing symlink %s in input", sname)
					var buf strings.Builder
					if err := tree.Dump(ctx, &buf); err != nil {
						t.Errorf("failed to dump: %v", err)
					}
					t.Logf("input tree:\n%s", buf.String())
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "%s: Symlink not found: %v", sname, err),
					}, nil
				}
				t.Logf("input %s is ok: target=%s", sname, sn.Target)
			}

			dd, err := fakere.Put(ctx, []byte("obj/mac/foo.o: ../../mac/foo.mm\n"))
			if err != nil {
				msg := fmt.Sprintf("failed to write obj/mac/foo.o.d: %v", err)
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
						Path:   "obj/mac/foo.o",
						Digest: d,
					},
					{
						Path:   "obj/mac/foo.o.d",
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
