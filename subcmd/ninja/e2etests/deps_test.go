// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
	"go.chromium.org/build/siso/toolsupport/makeutil"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
	nsjailpb "go.chromium.org/build/siso/toolsupport/nsjailutil/proto"
)

func TestBuild_Deps_Incremental(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	gccDeps := []string{"../../base/foo.cc", "../../base/foo.h"}
	msvcDeps := []string{"../../base/foo.h", "../../base/foo.cc"}
	depfileDeps := []string{"../../base/foo.cc", "../../base/foo.h"}

	checkDeps := func() (err error) {
		depsLog, err := ninjabuild.NewDepsLog(ctx, filepath.Join(dir, "out/siso/.siso_deps"))
		if err != nil {
			return fmt.Errorf("NewDepsLog: %w", err)
		}
		defer func() {
			cerr := depsLog.Close()
			if err == nil && cerr != nil {
				err = fmt.Errorf("depsLog.Close: %w", cerr)
			}
		}()
		var errs error
		got, _, err := depsLog.RetrievePaths(ctx, "foo.o")
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("deps for foo.o: %w", err))
		} else if !slices.Equal(got, gccDeps) {
			errs = errors.Join(errs, fmt.Errorf("deps for foo.o: got=%q want=%q", got, gccDeps))
		}

		got, _, err = depsLog.RetrievePaths(ctx, "foo.obj")
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("deps for foo.obj: %w", err))
		} else if !slices.Equal(got, msvcDeps) {
			errs = errors.Join(errs, fmt.Errorf("deps for foo.obj: got=%q want=%q", got, msvcDeps))
		}

		_, _, err = depsLog.RetrievePaths(ctx, "foo.out")
		if !errors.Is(err, ninjautil.ErrNoDepsLog) {
			errs = errors.Join(errs, fmt.Errorf("deps for foo.out: %w", err))
		}
		fsys := os.DirFS(dir)
		got, err = makeutil.ParseDepsFile(ctx, fsys, "out/siso/foo.out.d")
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("deps for foo.out.d: %w", err))
		} else if !slices.Equal(got, depfileDeps) {
			errs = errors.Join(errs, fmt.Errorf("deps for foo.out: got=%q want=%q", got, depfileDeps))
		}
		return errs
	}

	runNinjaTest := func(t *testing.T, fakere *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds build.DataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.Client = reapitest.New(ctx, t, fakere)
		ds.Cache = ds.Client.CacheStore()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			DataSource:  ds,
		})
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.Cache,
			EnableRead: true,
		})
		if err != nil {
			return build.Stats{}, err
		}
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			cmd := &rpb.Command{}
			err := fakere.FetchProto(ctx, action.CommandDigest, cmd)
			if err != nil {
				return nil, err
			}
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			fn, err := tree.LookupFileNode(ctx, "base/foo.cc")
			if err != nil {
				t.Logf("missing base/foo.cc: %v", err)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "../../base/foo.cc: File not found: %v", err),
				}, nil
			}
			if len(cmd.Arguments) < 2 {
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "unknown command line: %q", cmd.Arguments),
				}, nil
			}
			switch {
			case slices.Equal(cmd.Arguments[:2], []string{"python3", "../../tools/deps-gcc.py"}):
				d, err := fakere.Put(ctx, []byte("foo.o: ../../base/foo.cc ../../base/foo.h\n"))
				if err != nil {
					t.Logf("faked to write foo.o.d: %v", err)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "foo.o.d: failed to store %v", err),
					}, nil
				}
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "foo.o",
							Digest: fn.Digest,
						},
						{
							Path:   "foo.o.d",
							Digest: d,
						},
					},
				}, nil

			case slices.Equal(cmd.Arguments[:2], []string{"python3", "../../tools/deps-msvc.py"}):
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "foo.obj",
							Digest: fn.Digest,
						},
					},
					StdoutRaw: []byte("Note: including file: ../../base/foo.h\n"),
				}, nil
			case slices.Equal(cmd.Arguments[:2], []string{"python3", "../../tools/depfile.py"}):
				d, err := fakere.Put(ctx, []byte("foo.out: ../../base/foo.cc ../../base/foo.h\n"))
				if err != nil {
					t.Logf("faked to write foo.out.d: %v", err)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "foo.o.d: failed to store %v", err),
					}, nil
				}
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "foo.out",
							Digest: fn.Digest,
						},
						{
							Path:   "foo.out.d",
							Digest: d,
						},
					},
				}, nil
			default:
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "unknown command line: %q", cmd.Arguments),
				}, nil
			}
		},
	}

	t.Logf("-- first build")
	stats, err := runNinjaTest(t, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 4 {
		t.Errorf("done=%d total=%d; want done=total=4", stats.Done, stats.Total)
	}
	err = checkDeps()
	if err != nil {
		t.Errorf("checkDeps %v", err)
	}

	t.Logf("-- confirm no-op")
	stats, err = runNinjaTest(t, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 4 || stats.Skipped != 4 {
		t.Errorf("done=%d total=%d skipped=%d; want done=total=skipped=4, %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	touchFile(t, dir, "base/foo.cc")
	t.Logf("-- second build")
	stats, err = runNinjaTest(t, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 4 || stats.CacheHit+stats.Remote != 3 {
		t.Errorf("done=%d total=%d cache_hit=%d remote=%d; want done=total=4 cache_hit+remote=3: %#v", stats.Done, stats.Total, stats.CacheHit, stats.Remote, stats)
	}

	err = checkDeps()
	if err != nil {
		t.Errorf("checkDeps %v", err)
	}
}

// TestBuild_Deps_Stale checks ninja runs step if deps log is stale
// (foo.o is modified, so newer than mtime recorded in deps log).
func TestBuild_Deps_Stale(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	gccDeps := []string{"../../base/foo.cc", "../../base/foo.h"}

	checkDeps := func() (err error) {
		depsLog, err := ninjabuild.NewDepsLog(ctx, filepath.Join(dir, "out/siso/.siso_deps"))
		if err != nil {
			return fmt.Errorf("NewDepsLog: %w", err)
		}
		defer func() {
			cerr := depsLog.Close()
			if err == nil && cerr != nil {
				err = fmt.Errorf("depsLog.Close: %w", cerr)
			}
		}()
		got, gotKey, err := depsLog.RetrievePaths(ctx, "foo.o")
		if err != nil {
			return fmt.Errorf("deps for foo.o: %w", err)
		}
		fi, err := os.Stat(filepath.Join(dir, "out/siso/foo.o"))
		if err != nil {
			return fmt.Errorf("deps for foo.o: missing: %w", build.ErrStaleDeps)
		}
		if !fi.ModTime().Equal(gotKey.Mtime) {
			return fmt.Errorf("deps for foo.o: stale %v != %v: %w", fi.ModTime(), gotKey.Mtime, build.ErrStaleDeps)
		}
		if !slices.Equal(got, gccDeps) {
			return fmt.Errorf("deps for foo.o: got=%q want=%q", got, gccDeps)
		}
		return nil
	}

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			KeepTainted: true, // avoid recontime mtime of foo.o
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	t.Logf("-- first build")
	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d; want done=total=1", stats.Done, stats.Total)
	}
	err = checkDeps()
	if err != nil {
		t.Errorf("checkDeps %v", err)
	}
	t.Logf("-- confirm no-op")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Skipped != 1 {
		t.Errorf("done=%d total=%d skipped=%d; want done=total=skipped=1, %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	modifyFile(t, dir, "out/siso/foo.o", func(in []byte) []byte {
		return append(in, []byte("modified")...)
	})
	err = checkDeps()
	if !errors.Is(err, build.ErrStaleDeps) {
		t.Errorf("checkDeps %v; want %v", err, build.ErrStaleDeps)
	}

	t.Logf("-- second build")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Skipped != 0 {
		t.Errorf("done=%d total=%d skipped=%d; want done=total=1 skipped=0, %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
	err = checkDeps()
	if err != nil {
		t.Errorf("checkDeps %v", err)
	}
}

func TestBuild_Deps_SandboxPhonyExpand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sandbox is only available on linux")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	stats, err := runNinja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 3 {
		t.Errorf("done=%d total=%d; want done=total=3", stats.Done, stats.Total)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/nsjail.config"))
	if err != nil {
		t.Fatalf("read nsjail.config: %v", err)
	}
	config := &nsjailpb.NsJailConfig{}
	err = prototext.Unmarshal(buf, config)
	if err != nil {
		t.Fatalf("unmarshal nsjail.config: %v\n%s", err, buf)
	}
	if !slices.ContainsFunc(config.Mount, func(mount *nsjailpb.MountPt) bool {
		return mount.GetDst() == "/src/toybox"
	}) {
		t.Errorf("missing toybox\n%s", buf)
	}
	if !slices.ContainsFunc(config.Mount, func(mount *nsjailpb.MountPt) bool {
		return mount.GetDst() == "/src/in.1"
	}) {
		t.Errorf("missing in.1 (order_only phony)\n%s", buf)
	}
	if !slices.ContainsFunc(config.Mount, func(mount *nsjailpb.MountPt) bool {
		return mount.GetDst() == "/src/in.2"
	}) {
		t.Errorf("missing in.2 (order_only phony)\n%s", buf)
	}

}
