// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// fakeTwoPhaseCaching is a twoPhaseCaching whose lookups all hit or all miss.
type fakeTwoPhaseCaching struct {
	hit bool
}

func (fakeTwoPhaseCaching) ComputeLookupKey(context.Context, *Step) (string, error) {
	return "lookup-key", nil
}

func (f fakeTwoPhaseCaching) Check(context.Context, string, *Step) error {
	if f.hit {
		return nil
	}
	return errors.New("cache miss")
}

func (fakeTwoPhaseCaching) Add(context.Context, string, *Step) error { return nil }

// TestLazyExpand runs a step and checks that the command sees the
// expanded inputs and that a two phase cache hit does not expand them.
//
// runStep does not expand inputs. Execution starts in execLocal (after
// its two phase cache lookup) or in preprocCmd; both call
// ensureExpanded. The other strategies (fast local, racing, fallback)
// reach one of them before running the command.
//
// The command copies expanded.txt to out.txt. expanded.txt is only in
// hashfs, not on disk, and only ExpandedInputs returns it, so the
// command finds it only if it is in cmd.Inputs: prepareLocalInputs
// writes cmd.Inputs to disk, and the fake remote looks it up in the
// action's input root.
func TestLazyExpand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	SetExperimentForTest("two-phase-caching")
	t.Cleanup(func() { SetExperimentForTest("") })
	const content = "expanded content"

	for _, tc := range []struct {
		name       string
		remote     bool // pure with a platform, so runStrategy picks runRemote
		startLocal bool // runRemote runs execLocal without preprocCmd
		noTPC      bool // no two phase caching
		tpcHit     bool
		wantLocal  bool
	}{
		{name: "local_no_two_phase_caching", noTPC: true, wantLocal: true},
		{name: "local_two_phase_cache_miss", wantLocal: true},
		{name: "local_two_phase_cache_hit", tpcHit: true},
		{name: "remote", remote: true},
		{name: "remote_start_local", remote: true, startLocal: true, wantLocal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			// The fake remote does what the command does: it returns
			// expanded.txt from the input root as out.txt.
			fakere := &reapitest.Fake{
				ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
					tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
					f, err := tree.LookupFileNode(ctx, "expanded.txt")
					if err != nil {
						return &rpb.ActionResult{ExitCode: 1}, nil
					}
					return &rpb.ActionResult{
						OutputFiles: []*rpb.OutputFile{{Path: "out.txt", Digest: f.Digest}},
					}, nil
				},
			}
			env := newRacingTestEnv(t, fakere)
			now := time.Now()
			err := env.hashFS.Update(ctx, env.dir, []hashfs.UpdateEntry{{
				Name: "expanded.txt",
				Entry: &merkletree.Entry{
					Name: "expanded.txt",
					Data: blob.FromBytes(digest.SHA256, "expanded.txt", []byte(content)),
				},
				Mode:        0644,
				ModTime:     now,
				UpdatedTime: now,
			}})
			if err != nil {
				t.Fatal(err)
			}
			cache, err := NewCache(ctx, CacheOptions{Store: env.cachestore, EnableRead: true})
			if err != nil {
				t.Fatal(err)
			}
			b := env.newBuilder(t, func(opts *Options) {
				opts.Cache = cache
				opts.RECacheEnableRead = true
				opts.Limits.TwoPhaseCache = 1
				if tc.startLocal {
					opts.Limits.StartLocal = 1
				}
			})
			b.twoPhaseCaching = fakeTwoPhaseCaching{hit: tc.tpcHit}
			if tc.noTPC {
				b.twoPhaseCaching = nil
			}

			cmd := &execute.Cmd{
				Args:          []string{"/bin/sh", "-c", "cat expanded.txt > out.txt"},
				Outputs:       []path.Path{"out.txt"},
				WorkspaceRoot: env.dir,
				HashFS:        env.hashFS,
				CmdHash:       []byte("cmdhash"),
			}
			if tc.remote {
				cmd.Pure = true
				cmd.Platform = map[string]string{"container-image": "docker://ubuntu"}
			}
			cmd.InitOutputs()
			step := newRacingTestStep(cmd, []string{"out.txt"})
			var expansions atomic.Int32
			step.def = fakeStepDef{
				actionName: "gen",
				outputs:    path.Paths([]string{"out.txt"}),
				expandedInputs: func(context.Context) []path.Path {
					expansions.Add(1)
					return []path.Path{"expanded.txt"}
				},
			}

			err = b.runStrategy(step)(ctx, step)
			if err != nil {
				t.Fatalf("run=%v; want nil", err)
			}
			if step.metrics.IsLocal != tc.wantLocal {
				t.Errorf("IsLocal=%t; want %t", step.metrics.IsLocal, tc.wantLocal)
			}
			if tc.tpcHit {
				if got := expansions.Load(); got != 0 {
					t.Errorf("expanded %d times on a two phase cache hit; want 0", got)
				}
				return
			}
			if got := expansions.Load(); got != 1 {
				t.Errorf("expanded %d times; want 1", got)
			}
			got, err := os.ReadFile(filepath.Join(env.dir, "out.txt"))
			if err != nil || string(got) != content {
				t.Errorf("out.txt=%q, %v; want %q", got, err, content)
			}
		})
	}
}

// TestEnsureExpanded checks that ensureExpanded expands at most once,
// also on a clone, as on paths that reach both preprocCmd and execLocal
// (fallback, racing).
func TestEnsureExpanded(t *testing.T) {
	ctx := t.Context()
	env := newRacingTestEnv(t, &reapitest.Fake{})
	b := env.newBuilder(t, nil)
	expansions := 0
	step := newRacingTestStep(&execute.Cmd{WorkspaceRoot: env.dir, HashFS: env.hashFS}, nil)
	step.def = fakeStepDef{
		expandedInputs: func(context.Context) []path.Path {
			expansions++
			return nil
		},
	}

	ensureExpanded(ctx, b, step)
	ensureExpanded(ctx, b, step)
	ensureExpanded(ctx, b, step.Clone())
	if expansions != 1 {
		t.Errorf("expanded %d times; want 1", expansions)
	}
}
