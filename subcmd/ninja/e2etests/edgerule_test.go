// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_EdgeRule(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, ds build.DataSource) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	t.Logf("first build")
	// make sure replace / accumulate works
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			cmd := &rpb.Command{}
			err := fakere.FetchProto(ctx, action.CommandDigest, cmd)
			if err != nil {
				return nil, err
			}
			switch {
			case cmp.Equal(cmd.Arguments, []string{"python3", "../../tools/clang.py", "-c", "../../foo.cc", "-o", "obj/foo.o"}):
				tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
				fn, err := tree.LookupFileNode(ctx, "foo.cc")
				if err != nil {
					t.Logf("missing foo.cc: %v", err)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "../../foo.cc: File not found: %v", err),
					}, nil
				}
				_, err = tree.LookupFileNode(ctx, "out/x/gen/bar.h")
				if err != nil {
					t.Logf("gen/bar.h not found for foo.cc")
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "gen/bar.h: File not found: %v", err),
					}, nil
				}
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "obj/foo.o",
							Digest: fn.Digest,
						},
					},
				}, nil

			case cmp.Equal(cmd.Arguments, []string{"python3", "../../tools/clang.py", "-c", "gen/bar.cc", "-o", "obj/bar.o"}):
				tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
				fn, err := tree.LookupFileNode(ctx, "out/x/gen/bar.cc")
				if err != nil {
					t.Logf("gen/bar.cc not found by gen/bar.cc: %v", err)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "gen/bar.cc: File not found: %v", err),
					}, nil
				}
				_, err = tree.LookupFileNode(ctx, "out/x/gen/bar.h")
				if err != nil {
					t.Logf("gen/bar.h not found for gen/bar.cc")
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "gen/bar.h: File not found: %v", err),
					}, nil
				}
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "obj/bar.o",
							Digest: fn.Digest,
						},
					},
				}, nil

			case cmp.Equal(cmd.Arguments, []string{"python3", "../../tools/ar.py", "-c", "obj/bar.a", "obj/bar.o"}):
				tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
				_, err := tree.LookupFileNode(ctx, "out/x/obj/bar.o")
				if err != nil {
					t.Logf("missing obj/bar.o: %v", err)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "obj/bar.o: File not found: %v", err),
					}, nil
				}
				d, err := fakere.Put(ctx, []byte("obj/bar.o\n"))
				if err != nil {
					t.Logf("failed to write obj/bar.a: %v", err)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "obj/bar.a: failed to store %v", err),
					}, nil
				}
				t.Logf("obj/bar.a => %s", d)
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "obj/bar.a",
							Digest: d,
						},
					},
				}, nil

			case cmp.Equal(cmd.Arguments, []string{"python3", "../../tools/link.py", "obj/foo.o", "obj/bar.a", "-o", "obj/foo.so"}):
				var buf bytes.Buffer
				tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
				for _, input := range []string{"obj/foo.o", "obj/bar.a"} {
					fn, err := tree.LookupFileNode(ctx, path.Join("out/x", input))
					if err != nil {
						t.Logf("missing %s: %v", input, err)
						return &rpb.ActionResult{
							ExitCode:  1,
							StderrRaw: fmt.Appendf(nil, "%s: File not found: %v", input, err),
						}, nil
					}
					fmt.Fprintln(&buf, input)
					data, err := fakere.Fetch(ctx, fn.Digest)
					if err != nil {
						return &rpb.ActionResult{
							ExitCode:  1,
							StderrRaw: fmt.Appendf(nil, "%s: File content not found: %v", input, err),
						}, nil
					}
					buf.Write(data)
				}
				// obj/bar.a is thin archive, so it needs obj/bar.o too
				_, err := tree.LookupFileNode(ctx, "out/x/obj/bar.o")
				if err != nil {
					t.Logf("obj/bar.o not found for obj/bar.a")
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "obj/bar.o: File not found: %v", err),
					}, nil
				}
				d, err := fakere.Put(ctx, buf.Bytes())
				if err != nil {
					t.Logf("failed to write obj/foo.so: %v", err)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "obj/foo.so: failed to store: %v", err),
					}, nil
				}
				t.Logf("obj/foo.so => %s", d)
				return &rpb.ActionResult{
					ExitCode: 0,
					OutputFiles: []*rpb.OutputFile{
						{
							Path:   "obj/foo.so",
							Digest: d,
						},
					},
				}, nil

			default:
				return nil, fmt.Errorf("unsupported commandline %q", cmd.Arguments)
			}
		},
	}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.Client = reapitest.New(ctx, t, fakere)
	ds.Cache = ds.Client.CacheStore()

	stats, err := runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 7 || stats.Done != stats.Total || stats.Remote != 4 || stats.Skipped != 1 || stats.LocalFallback != 0 {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d local_fallback=%d; want total=7 done=7 remote=4 skipped=1 local_fallback=0 (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats.LocalFallback, stats)
	}

	t.Logf("second build. no-op")
	stats, err = runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 7 || stats.Done != stats.Total || stats.Remote != 0 || stats.Skipped != stats.Total {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d; want total=7 done=7 remote=0 skipped=7", stats.Total, stats.Done, stats.Remote, stats.Skipped)
	}

	modifyFile(t, dir, "foo.cc", func([]byte) []byte {
		return []byte(`
// new content
#include "gen/bar.h"
`)
	})

	t.Logf("third build")
	// make sure replace / accumulate still works
	// even if stamp, alink steps are skipped.
	stats, err = runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	// remote: cxx obj/foo.o, solink obj/foo.so
	if stats.Total != 7 || stats.Done != stats.Total || stats.Remote != 2 || stats.Skipped != 5 || stats.LocalFallback != 0 {
		t.Errorf("stats total=%d done=%d remote=%d skipped=%d local_fallback=%d; want total=7 done=7 remote=2 skipped=5 local_fallback=0: (%#v)", stats.Total, stats.Done, stats.Remote, stats.Skipped, stats.LocalFallback, stats)
	}
}

func TestBuild_EdgeRule_solibs(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, ds build.DataSource) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	t.Logf("first build")
	// make sure solibs is used in input.
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			for _, input := range []string{
				"../../protobuf/protoc_wrapper.py",
				"protoc",
				"libc++.so",
				"../../protobuf/foo.proto",
			} {
				_, err := tree.LookupFileNode(ctx, path.Join("out/x", input))
				t.Logf("input %s: %v", input, err)
				if err != nil {
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "%s: File not found: %v", input, err),
					}, nil
				}
			}
			var outputs []*rpb.OutputFile
			for _, output := range []string{
				"gen/foo.pb.h",
				"gen/foo.pb.cc",
			} {
				d, err := fakere.Put(ctx, fmt.Appendf(nil, "generate %s from ../../protobuf/foo.proto", output))
				if err != nil {
					msg := fmt.Sprintf("failed to write %s: %v", output, err)
					t.Log(msg)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(msg),
					}, nil
				}
				outputs = append(outputs, &rpb.OutputFile{
					Path:   output,
					Digest: d,
				})
			}
			return &rpb.ActionResult{
				ExitCode:    0,
				OutputFiles: outputs,
			}, nil
		},
	}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.Client = reapitest.New(ctx, t, fakere)
	ds.Cache = ds.Client.CacheStore()

	stats, err := runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 3 || stats.Done != stats.Total || stats.Remote != 1 || stats.Local != 1 || stats.LocalFallback != 0 {
		t.Errorf("done=%d remote=%d local=%d local_fallback=%d total=%d; want done=total=3 remote=1 local=1 local_fallback=0; %#v", stats.Done, stats.Remote, stats.Local, stats.LocalFallback, stats.Total, stats)
	}

	t.Logf("second build. no-op")
	stats, err = runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 3 || stats.Done != stats.Total || stats.Skipped != stats.Total {
		t.Errorf("done=%d skipped=%d total=%d; want done=total=skipped=3; %#v", stats.Done, stats.Skipped, stats.Total, stats)
	}

	modifyFile(t, dir, "protobuf/foo.proto", func([]byte) []byte {
		return []byte(`
// new content
`)
	})

	t.Logf("third build")
	// make sure solibs still works
	stats, err = runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 3 || stats.Done != stats.Total || stats.Remote != 1 || stats.Local != 0 || stats.LocalFallback != 0 {
		t.Errorf("done=%d remote=%d local=%d local_fallback=%d total=%d; want done=total=3 remote=1 local=0 local_fallback=0; %#v", stats.Done, stats.Remote, stats.Local, stats.LocalFallback, stats.Total, stats)
	}
}

func TestBuild_EdgeRule_solibs_recursive(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, ds build.DataSource) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	t.Logf("first build")
	// make sure solibs is used in input.
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			for _, input := range []string{
				"../../build/gn_run_binary.py",
				"generate_fontconfig_caches",
				"libthird_party_fontconfig.so",
				"libc++.so",
				"libthird_party_freetype_harfbuzz.so",
				"../../test_fonts.ttf",
			} {
				_, err := tree.LookupFileNode(ctx, path.Join("out/x", input))
				t.Logf("input %s: %v", input, err)
				if err != nil {
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "%s: File not found: %v", input, err),
					}, nil
				}
			}
			var outputs []*rpb.OutputFile
			for _, output := range []string{
				"fontconfig_caches/cache",
			} {
				d, err := fakere.Put(ctx, []byte("generated by generate_fontconfig_caches"))
				if err != nil {
					msg := fmt.Sprintf("failed to write %s: %v", output, err)
					t.Log(msg)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(msg),
					}, nil
				}
				outputs = append(outputs, &rpb.OutputFile{
					Path:   output,
					Digest: d,
				})
			}
			return &rpb.ActionResult{
				ExitCode:    0,
				OutputFiles: outputs,
			}, nil
		},
	}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.Client = reapitest.New(ctx, t, fakere)
	ds.Cache = ds.Client.CacheStore()

	stats, err := runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 5 || stats.Done != stats.Total || stats.Remote != 1 || stats.Local != 3 || stats.LocalFallback != 0 {
		t.Errorf("done=%d remote=%d local=%d local_fallback=%d total=%d; want done=total=5 remote=1 local=3 local_fallback=0; %#v", stats.Done, stats.Remote, stats.Local, stats.LocalFallback, stats.Total, stats)
	}

	t.Logf("second build. no-op")
	stats, err = runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 5 || stats.Done != stats.Total || stats.Skipped != stats.Total {
		t.Errorf("done=%d skipped=%d total=%d; want done=total=skipped=5; %#v", stats.Done, stats.Skipped, stats.Total, stats)
	}

	modifyFile(t, dir, "test_fonts.ttf", func([]byte) []byte {
		return []byte(`
// new fonts
`)
	})

	t.Logf("third build")
	// make sure solibs still works
	stats, err = runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 5 || stats.Done != stats.Total || stats.Remote != 1 || stats.Local != 0 || stats.LocalFallback != 0 {
		t.Errorf("done=%d remote=%d local=%d local_fallback=%d total=%d; want done=total=5 remote=1 local=0 local_fallback=0; %#v", stats.Done, stats.Remote, stats.Local, stats.LocalFallback, stats.Total, stats)
	}
}

func TestBuild_EdgeRule_stamp_solibs(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, ds build.DataSource) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	t.Logf("-- first build")
	// make sure solibs is used in input.
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			t.Logf("action %#v", action)
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			for _, input := range []string{
				"../../build/gn_run_binary.py",
				"flatc",
				"libc++.so",
				"../../include.fbs",
			} {
				_, err := tree.LookupFileNode(ctx, path.Join("out/x", input))
				if err != nil {
					t.Logf("err for %s: %v", input, err)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: fmt.Appendf(nil, "%s: File not found: %v", input, err),
					}, nil
				}
			}
			var outputs []*rpb.OutputFile
			for _, output := range []string{
				"gen/generated.h",
			} {
				d, err := fakere.Put(ctx, []byte("generated by flatc"))
				if err != nil {
					msg := fmt.Sprintf("failed to write %s: %v", output, err)
					t.Log(msg)
					return &rpb.ActionResult{
						ExitCode:  1,
						StderrRaw: []byte(msg),
					}, nil
				}
				outputs = append(outputs, &rpb.OutputFile{
					Path:   output,
					Digest: d,
				})
			}
			t.Logf("%#v", outputs)
			return &rpb.ActionResult{
				ExitCode:    0,
				OutputFiles: outputs,
			}, nil
		},
	}

	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.Client = reapitest.New(ctx, t, fakere)
	ds.Cache = ds.Client.CacheStore()

	stats, err := runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NoExec != 1 || stats.Local != 1 || stats.Remote != 1 || stats.Done != 4 || stats.Total != 4 {
		t.Logf("no_exec=%d local=%d remote=%d done=%d total=%d; want no_exec=1 local=1 remote=1 done=total=4", stats.NoExec, stats.Local, stats.Remote, stats.Done, stats.Total)
	}

	t.Logf("-- second build. no-op")
	stats, err = runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total {
		t.Errorf("done=%d skipped=%d total=%d; want done=total=skipped=%v; %#v", stats.Done, stats.Skipped, stats.Total, stats.Total, stats)
	}

	modifyFile(t, dir, "include.fbs", func([]byte) []byte {
		return []byte(`
// new include.fbs
`)
	})

	t.Logf("-- third build")
	// make sure solibs still works
	stats, err = runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NoExec != 0 || stats.Local != 0 || stats.Remote != 1 || stats.Done != 4 || stats.Total != 4 {
		t.Logf("no_exec=%d local=%d remote=%d done=%d total=%d; want no_exec=0 local=0 remote=1 done=total=4", stats.NoExec, stats.Local, stats.Remote, stats.Done, stats.Total)
	}
}

func TestBuild_EdgeRule_StrictRemote(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, ds build.DataSource) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		// Enable StartLocal to try to run it locally first.
		opt.Limits.StartLocal = 5
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	writeFile := func(t *testing.T, fname, content string) {
		t.Helper()
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, filepath.Join(dir, "build/config/siso/main.star"), `
load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    step_config = {
        "rules": [
            {
                "name": "cxx/compile",
                "action": "cxx",
                "remote": True,
                "strict_remote": True,
                "platform": {
                    "container-image": "docker://gcr.io/test/test",
                },
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {},
    )
`)
	writeFile(t, filepath.Join(dir, "out/siso/build.ninja"), `
rule cxx
  command = echo cxx > ${out}

build obj/foo.o: cxx ../../foo.cc
build all: phony obj/foo.o
build build.ninja: phony
`)
	writeFile(t, filepath.Join(dir, "foo.cc"), "int a;")

	remoteExecuted := false
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			remoteExecuted = true
			d, err := fakere.Put(ctx, []byte("remote output"))
			if err != nil {
				return nil, err
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "obj/foo.o",
						Digest: d,
					},
				},
			}, nil
		},
	}

	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.Client = reapitest.New(ctx, t, fakere)
	ds.Cache = ds.Client.CacheStore()

	stats, err := runNinjaTest(t, ds)
	if err != nil {
		t.Fatal(err)
	}

	if !remoteExecuted {
		t.Errorf("Step was not executed remotely even though strict_remote was set.")
	}
	if stats.Remote != 1 {
		t.Errorf("stats.Remote = %d; want 1. stats: %#v", stats.Remote, stats)
	}
	if stats.Local != 0 {
		t.Errorf("stats.Local = %d; want 0 (should bypass StartLocal). stats: %#v", stats.Local, stats)
	}
}
