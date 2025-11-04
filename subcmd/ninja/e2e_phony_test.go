// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_PhonyDir(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 3 {
		t.Errorf("done=%d local=%d; want done=5 local=3", stats.Done, stats.Local)
	}

	resContent := []byte("new res.bin")
	modifyFile(t, dir, "res.bin", func([]byte) []byte {
		return resContent
	})

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 3 {
		t.Errorf("done=%d local=%d; want done=5 local=3", stats.Done, stats.Local)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo.app/Contents/Frameworks/Foo Framework.framework/Versions/C/Resources/res.bin"))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(buf, resContent) {
		t.Errorf("unexpected content=%q; want=%q", buf, resContent)
	}

	t.Logf("third build. should be no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 {
		t.Errorf("done=%d local=%d; want done=5 local=0", stats.Done, stats.Local)
	}
}

func TestBuild_PhonyDirCopyHandler(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 1 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=2 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	resContent := []byte("new res.bin")
	modifyFile(t, dir, "res.bin", func([]byte) []byte {
		return resContent
	})

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 1 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=2 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo.app/Contents/Frameworks/Foo Framework.framework/Versions/C/Resources/res.bin"))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(buf, resContent) {
		t.Errorf("unexpected content=%q; want=%q", buf, resContent)
	}

	t.Logf("third build. should be no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 {
		t.Errorf("done=%d local=%d; want done=5 local=0", stats.Done, stats.Local)
	}
}

func TestBuild_PhonyDirStampHandler(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 1 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=2 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	resContent := []byte("new res.bin")
	modifyFile(t, dir, "res.bin", func([]byte) []byte {
		return resContent
	})

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 1 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=2 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo.app/Contents/Frameworks/Foo Framework.framework/Versions/C/Resources/res.bin"))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(buf, resContent) {
		t.Errorf("unexpected content=%q; want=%q", buf, resContent)
	}

	t.Logf("third build. should be no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 {
		t.Errorf("done=%d local=%d; want done=5 local=0", stats.Done, stats.Local)
	}
}

func TestBuild_PhonyDirStampCopyHandler(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 1 || stats.NoExec != 2 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=1 no_exec=1", stats.Done, stats.Local, stats.NoExec)
	}

	resContent := []byte("new res.bin")
	modifyFile(t, dir, "res.bin", func([]byte) []byte {
		return resContent
	})

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 1 || stats.NoExec != 2 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=1 no_exec=2", stats.Done, stats.Local, stats.NoExec)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo.app/Contents/Frameworks/Foo Framework.framework/Versions/C/Resources/res.bin"))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(buf, resContent) {
		t.Errorf("unexpected content=%q; want=%q", buf, resContent)
	}

	t.Logf("third build. should be no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 {
		t.Errorf("done=%d local=%d; want done=5 local=0", stats.Done, stats.Local)
	}
}

func TestBuild_PhonyStamp(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 3 || stats.NoExec != 0 {
		t.Errorf("done=%d local=%d no_exec=%d;  want done=5 local=3 no_exec=0", stats.Done, stats.Local, stats.NoExec)
	}

	newContent := []byte("new 0.input")
	modifyFile(t, dir, "foo/0.input", func([]byte) []byte {
		return newContent
	})

	t.Logf("second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 2 || stats.NoExec != 0 {
		t.Errorf("done=%d local=%d no_exec=%d;  want done=5 local=2 no_exec=0", stats.Done, stats.Local, stats.NoExec)
	}

	t.Logf("confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 0 || stats.NoExec != 0 {
		t.Errorf("done=%d local=%d no_exec=%d; want done=5 local=0 no_exec=0", stats.Done, stats.Local, stats.NoExec)
	}
}

func TestBuild_PhonyReplace(t *testing.T) {
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

	refake := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			_, err := tree.LookupFileNode(ctx, "cp.py")
			if err != nil {
				t.Logf("missing cp.py: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "../../cp.py: File not found: %v", err),
				}, nil
			}
			fn, err := tree.LookupFileNode(ctx, "foo.in")
			if err != nil {
				t.Logf("missing foo.in: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "../../foo.in: File not found: %v", err),
				}, nil
			}
			_, err = tree.LookupFileNode(ctx, "foo2.in")
			if err != nil {
				t.Logf("missing foo2.in: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "../../foo2.in: File not found: %v", err),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "foo.out",
						Digest: fn.Digest,
					},
				},
			}, nil
		},
	}

	stats, err := ninja(t, refake)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 4 || stats.Done != stats.Total || stats.Remote != 1 || stats.Skipped != 3 || stats.LocalFallback != 0 {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d local_fallback=%d; want total=4 done=4 remote=1 skipped=3 local_fallback=0 (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats.LocalFallback, stats)
	}

	t.Logf("-- second build. no-op")
	stats, err = ninja(t, refake)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 4 || stats.Done != stats.Total || stats.Remote != 0 || stats.Skipped != stats.Total {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d; want total=4 done=4 remote=0 skipped=4 (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats)
	}
}

func TestBuild_PhonyIndirectInputs(t *testing.T) {
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

	refake := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			cmd := &rpb.Command{}
			err := fakere.FetchProto(ctx, action.CommandDigest, cmd)
			if err != nil {
				return nil, err
			}
			if len(cmd.Arguments) < 3 {
				t.Logf("wrong arguments %q", cmd.Arguments)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "wrong arguments %q", cmd.Arguments),
				}, nil
			}
			if !slices.Equal(cmd.Arguments[:2], []string{"python3", "../../mojom_parser.py"}) {
				t.Logf("wrong command %q", cmd.Arguments)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "wrong command %q", cmd.Arguments),
				}, nil
			}
			var outputFiles []*rpb.OutputFile
			switch cmd.Arguments[2] {
			case "foo":
				d, err := fakere.Put(ctx, []byte("foo.mojom-module"))
				if err != nil {
					return nil, err
				}
				outputFiles = append(outputFiles, &rpb.OutputFile{
					Path:   "foo.mojom-module",
					Digest: d,
				})
			case "absl_status":
				d, err := fakere.Put(ctx, []byte("absl_status.mojom-module"))
				if err != nil {
					return nil, err
				}
				outputFiles = append(outputFiles, &rpb.OutputFile{
					Path:   "gen/absl_status.mojom-module",
					Digest: d,
				})
			default:
				t.Logf("wrong command %q", cmd.Arguments)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "wrong option %q", cmd.Arguments),
				}, nil
			}

			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			_, err = tree.LookupFileNode(ctx, "mojom_parser.py")
			if err != nil {
				t.Logf("missing mojom_parser.py: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "../../mojom_parser.py: File not found: %v", err),
				}, nil
			}
			_, err = tree.LookupFileNode(ctx, "out/siso/gen/base.build_metadata")
			if err != nil {
				t.Logf("missing out/siso/gen/build_metadata.py: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "gen/build_metadata.py: File not found: %v", err),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode:    0,
				OutputFiles: outputFiles,
			}, nil
		},
	}

	stats, err := ninja(t, refake)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 6 || stats.Done != stats.Total || stats.Remote != 2 || stats.Skipped != 3 || stats.LocalFallback != 0 {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d local_fallback=%d; want total=6 done=6 remote=2 skipped=3 local_fallback=0 (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats.LocalFallback, stats)
	}

	t.Logf("-- second build. no-op")
	stats, err = ninja(t, refake)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 6 || stats.Done != stats.Total || stats.Remote != 0 || stats.Skipped != stats.Total {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d; want total=6 done=6 remote=0 skipped=6 (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats)
	}
}

func TestBuild_PhonyDirty(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"obj/foo.o"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Local != 1 {
		t.Errorf("done=%d total=%d local=%d; want done=2 total=2 local=1; %#v", stats.Done, stats.Total, stats.Local, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Local != 0 || stats.Skipped != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=0 skipped=2; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	modifyFile(t, dir, "afdo.prof", func(b []byte) []byte {
		return append(b, []byte("\nupdated\n")...)
	})

	t.Logf("-- second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Local != 1 {
		t.Errorf("done=%d total=%d local=%d; want done=2 total=2 local=1; %#v", stats.Done, stats.Total, stats.Local, stats)
	}
}
