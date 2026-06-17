// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// TestAdoptRacingLocalResultPreservesWeightedDuration checks that a local
// race win keeps the original step's accumulated weighted duration rather
// than overwriting it with the never-ticked clone's zero value.
func TestAdoptRacingLocalResultPreservesWeightedDuration(t *testing.T) {
	const want = 5 * time.Second

	step := &Step{
		cmd:     &execute.Cmd{},
		state:   &stepState{},
		metrics: StepMetric{},
	}
	// The progress ticker only accumulates onto the original step, and
	// done() records it into the metrics.
	step.addWeightedDuration(want)
	step.metrics.WeightedDuration = IntervalMetric(step.getWeightedDuration())

	// The clone is never registered with the ticker, so its weighted
	// duration is zero.
	localStep := step.Clone()
	localStep.metrics.WeightedDuration = IntervalMetric(localStep.getWeightedDuration())
	if got := localStep.getWeightedDuration(); got != 0 {
		t.Fatalf("clone weighted duration = %v, want 0 (clone is never ticked)", got)
	}

	step.adoptRacingLocalResult(localStep)

	if got := step.getWeightedDuration(); got != want {
		t.Errorf("step.state.weightedDuration = %v, want %v", got, want)
	}
	if got := time.Duration(step.metrics.WeightedDuration); got != want {
		t.Errorf("step.metrics.WeightedDuration = %v, want %v", got, want)
	}
}

func TestIsContextCanceledErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "context.Canceled",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "wrapped context.Canceled",
			err:  fmt.Errorf("something: %w", context.Canceled),
			want: true,
		},
		{
			name: "grpc Canceled status",
			err:  status.Error(codes.Canceled, "context canceled"),
			want: true,
		},
		{
			name: "wrapped grpc Canceled status",
			err:  fmt.Errorf("find missing: %w", status.Error(codes.Canceled, "context canceled")),
			want: true,
		},
		{
			name: "deeply wrapped grpc Canceled (CAS upload path)",
			err: fmt.Errorf("failed to upload all foo: %w",
				fmt.Errorf("wait for digest=abc/123: %w",
					fmt.Errorf("find missing: %w",
						status.Error(codes.Canceled, "context canceled")))),
			want: true,
		},
		{
			name: "grpc DeadlineExceeded status",
			err:  status.Error(codes.DeadlineExceeded, "deadline exceeded"),
			want: false,
		},
		{
			name: "grpc Unavailable status",
			err:  status.Error(codes.Unavailable, "unavailable"),
			want: false,
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("something went wrong"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := isContextCanceledErr(tt.err), tt.want; got != want {
				t.Errorf("isContextCanceledErr(%v) = %v, want %v", tt.err, got, want)
			}
		})
	}
}

func TestRemoteClaimFallbackIfAllowed(t *testing.T) {
	tests := []struct {
		name               string
		err                error
		maxFallbackAllowed int64
		initialFallbacks   int64
		failuresAllowed    int
		cmdStdout          string
		wantOk             bool
		checkErr           func(*testing.T, error)
	}{
		{
			name:               "ContextCanceled",
			err:                context.Canceled,
			maxFallbackAllowed: 10,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("got %v, expected context.Canceled", err)
				}
			},
		},
		{
			name:               "DeadlineExceeded",
			err:                context.DeadlineExceeded,
			maxFallbackAllowed: 10,
			wantOk:             true,
			checkErr: func(t *testing.T, err error) {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			},
		},
		{
			name:               "ErrNotRelocatable",
			err:                errNotRelocatable,
			maxFallbackAllowed: 10,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, errNotRelocatable) {
					t.Errorf("got %v, expected errNotRelocatable", err)
				}
			},
		},
		{
			name:               "ErrNotInsideWorkspace",
			err:                errNotInsideWorkspace,
			maxFallbackAllowed: 10,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, errNotInsideWorkspace) {
					t.Errorf("got %v, expected errNotInsideWorkspace", err)
				}
			},
		},
		{
			name:               "ExitError_DefaultAllowFallback",
			err:                execute.ExitError{ExitCode: 1},
			maxFallbackAllowed: 10,
			wantOk:             true,
			checkErr: func(t *testing.T, err error) {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			},
		},
		{
			name:               "ExitError_PreferNoFallback",
			err:                execute.ExitError{ExitCode: 1},
			maxFallbackAllowed: 10,
			failuresAllowed:    1,
			cmdStdout:          "compile error",
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				var exitErr execute.ExitError
				if !errors.As(err, &exitErr) {
					t.Errorf("got %T: %v, expected execute.ExitError", err, err)
				} else if exitErr.ExitCode != 1 {
					t.Errorf("got exit code %d, expected 1", exitErr.ExitCode)
				}
			},
		},
		{
			name:               "ExitError_SIGKILL_AlwaysFallback",
			err:                execute.ExitError{ExitCode: 137},
			maxFallbackAllowed: 10,
			failuresAllowed:    1,
			cmdStdout:          "oom",
			wantOk:             true,
			checkErr: func(t *testing.T, err error) {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			},
		},
		{
			name:               "FallbackLimitExceeded",
			err:                status.Error(codes.Unavailable, "unavailable"),
			maxFallbackAllowed: 1,
			initialFallbacks:   1,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				var fallbackErr TooManyFallbackError
				wantErr := status.Error(codes.Unavailable, "unavailable")
				if !errors.As(err, &fallbackErr) {
					t.Errorf("got %T: %v, expected TooManyFallbackError", err, err)
				} else if !errors.Is(fallbackErr.Err, wantErr) {
					t.Errorf("got %v, expected wrapped error %v", fallbackErr.Err, wantErr)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			b := &Builder{
				maxFallbackAllowed: tt.maxFallbackAllowed,
			}
			b.numFallback.Store(tt.initialFallbacks)
			b.failures.allowed = tt.failuresAllowed
			if b.failures.allowed == 0 {
				b.failures.allowed = 2
			}

			cmd := &execute.Cmd{
				Args: []string{"clang++", "foo.cc"},
			}
			cmd.SetActionDigest(digest.Digest{Hash: "dummy", SizeBytes: 100})
			if tt.cmdStdout != "" {
				_, _ = cmd.StdoutWriter().Write([]byte(tt.cmdStdout))
			}

			step := &Step{
				outputPaths: []string{"foo.o"},
				cmd:         cmd,
				def:         fakeStepDef{},
			}

			ok, err := b.remoteClaimFallbackIfAllowed(ctx, step, tt.err)

			if ok != tt.wantOk {
				t.Errorf("remoteClaimFallbackIfAllowed() ok = %t; want %t", ok, tt.wantOk)
			}

			if tt.checkErr != nil {
				tt.checkErr(t, err)
			}
		})
	}
}

// racingTestEnv bundles the scaffolding shared by the runRacing tests:
// a temp dir, a fake REAPI client, a local cache store, and a hashfs
// with OutputLocal enabled.
type racingTestEnv struct {
	dir        string
	reclient   *reapi.Client
	cachestore *LocalCache
	hashFS     *hashfs.HashFS
}

func newRacingTestEnv(t *testing.T, fakere *reapitest.Fake) *racingTestEnv {
	t.Helper()
	ctx := t.Context()
	// t.Context() is canceled before t.Cleanup functions run.
	cleanupCtx := context.WithoutCancel(ctx)
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	reclient := reapitest.New(ctx, t, fakere)
	cachestore, err := NewLocalCache(filepath.Join(dir, ".siso_cache"))
	if err != nil {
		t.Fatal(err)
	}
	var ds DataSource
	ds.Client = reclient
	ds.Cache = cachestore
	t.Cleanup(func() {
		err := ds.Close(cleanupCtx)
		if err != nil {
			t.Error(err)
		}
	})
	hashFS, err := hashfs.New(ctx, hashfs.Option{
		DataSource:  ds,
		OutputLocal: func(context.Context, string) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hashFS.Close(cleanupCtx) })
	return &racingTestEnv{
		dir:        dir,
		reclient:   reclient,
		cachestore: cachestore,
		hashFS:     hashFS,
	}
}

// newBuilder creates a Builder with the racing test defaults
// (one local slot so the test controls who wins the race);
// mod, if non-nil, adjusts the options before the Builder is created.
func (env *racingTestEnv) newBuilder(t *testing.T, mod func(*Options)) *Builder {
	t.Helper()
	opts := Options{
		Path:         NewPath(env.dir, "out/siso"),
		HashFS:       env.hashFS,
		REAPIClient:  env.reclient,
		REExecEnable: true,
		Limits: Limits{
			Step:    10,
			Local:   1,
			Remote:  10,
			Preproc: 10,
			Cache:   10,
		},
	}
	if mod != nil {
		mod(&opts)
	}
	b, err := New(t.Context(), fakeGraph{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newRacingTestStep(cmd *execute.Cmd, outputs []string) *Step {
	return &Step{
		outputPaths:    outputs,
		cmd:            cmd,
		weight:         1,
		state:          &stepState{},
		startReported:  new(sync.Once),
		finishReported: new(sync.Once),
		def: fakeStepDef{
			actionName: "clang",
			outputs:    outputs,
		},
	}
}

// TestRunRacing_StaleLocalReadyOutput checks that a racing remote win
// self-heals a stale local-ready hashfs entry whose file was removed from
// disk behind siso's back (e.g. by a pre-build cleanup step after
// .siso_fs_state was loaded). b/522434556
//
// The scenario:
//  1. hashfs has a local-ready entry for stubs.jar (lready closed),
//     but the file is missing on disk.
//  2. The remote cache hit for the step fails to flush (chtimes on the
//     missing file), so runRacing treats it as a cache miss and races.
//  3. The only local semaphore slot is held by the test, so the local
//     racer never starts the command (cmdRunTime stays zero) and the
//     remote racer wins.
//  4. runRacing must forget the stale entry so that b.outputs()
//     re-materializes stubs.jar from CAS instead of failing with
//     "failed to flush outputs to local".
func TestRunRacing_StaleLocalReadyOutput(t *testing.T) {
	ctx := t.Context()
	content := []byte("stubs content")
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			dg, err := fakere.Put(ctx, content)
			if err != nil {
				return nil, err
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "stubs.jar",
						Digest: dg,
					},
				},
			}, nil
		},
	}
	env := newRacingTestEnv(t, fakere)
	dir := env.dir
	hashFS := env.hashFS

	// Simulate .siso_fs_state retention: a local-ready entry (lready
	// closed because the file exists on disk) with an old mtime.
	stubsPath := filepath.Join(dir, "stubs.jar")
	err := os.WriteFile(stubsPath, content, 0644)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-1 * time.Hour)
	err = os.Chtimes(stubsPath, oldTime, oldTime)
	if err != nil {
		t.Fatal(err)
	}

	cmd := &execute.Cmd{
		Args:          []string{"clang", "-o", "stubs.jar"},
		Outputs:       []string{"stubs.jar"},
		WorkspaceRoot: dir,
		HashFS:        hashFS,
		Platform:      map[string]string{"container-image": "docker://ubuntu"},
		Pure:          true,
		CmdHash:       []byte("cmdhash"),
	}
	cmd.InitOutputs()
	actionDigest, err := cmd.Digest(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = hashFS.Update(ctx, dir, []hashfs.UpdateEntry{
		{
			Name: "stubs.jar",
			Entry: &merkletree.Entry{
				Name: "stubs.jar",
				Data: digest.FromBytes("stubs.jar", content),
			},
			IsLocal:     true,
			Mode:        0644,
			ModTime:     oldTime,
			CmdHash:     cmd.CmdHash,
			Action:      actionDigest,
			UpdatedTime: oldTime,
			IsChanged:   false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Remove the file behind hashfs' back. The entry remains local-ready.
	err = os.Remove(stubsPath)
	if err != nil {
		t.Fatal(err)
	}

	// Prepare a remote cache hit with the same content but a newer mtime,
	// so that the cache-hit flush attempts chtimes on the missing file.
	dg, err := fakere.Put(ctx, content)
	if err != nil {
		t.Fatal(err)
	}
	actionResult := &rpb.ActionResult{
		ExitCode: 0,
		OutputFiles: []*rpb.OutputFile{
			{
				Path:   "stubs.jar",
				Digest: dg,
			},
		},
	}
	err = env.cachestore.SetActionResult(ctx, actionDigest, actionResult)
	if err != nil {
		t.Fatal(err)
	}
	err = env.cachestore.SetContent(ctx, digest.FromProto(dg), "stubs.jar", content)
	if err != nil {
		t.Fatal(err)
	}

	cache, err := NewCache(ctx, CacheOptions{
		Store:      env.cachestore,
		EnableRead: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	b := env.newBuilder(t, func(opts *Options) {
		opts.RECacheEnableRead = true
		opts.Cache = cache
	})

	step := newRacingTestStep(cmd, []string{"stubs.jar"})

	// Hold the only local semaphore slot so the local racer can never
	// start the command and the remote racer deterministically wins.
	err = b.localSema.Do(ctx, 1, func(ctx context.Context) error {
		return b.runRacing(ctx, step)
	})
	if err != nil {
		t.Fatalf("runRacing=%v; want nil err", err)
	}
	if got, want := step.metrics.RacingWinner, "remote"; got != want {
		t.Errorf("RacingWinner=%q; want %q", got, want)
	}
	got, err := os.ReadFile(stubsPath)
	if err != nil {
		t.Fatalf("stubs.jar not re-materialized on disk: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("stubs.jar content=%q; want %q", got, content)
	}
}

// TestRunRacing_RemoteWinFlushFailureFallsBackToLocal checks that when
// the remote racer wins but flushing its outputs to the local disk fails
// (e.g. the action result references a blob that is missing from CAS),
// runRacing falls back to local execution to regenerate the outputs,
// like runRemote does, instead of failing the build.
func TestRunRacing_RemoteWinFlushFailureFallsBackToLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	ctx := t.Context()
	content := []byte("stubs content")
	// The remote action result references the digest of content, but the
	// blob is never uploaded to CAS, so flushing the remote outputs to
	// the local disk fails with NotFound.
	missing := digest.FromBytes("stubs.jar", content).Digest()
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "stubs.jar",
						Digest: missing.Proto(),
					},
				},
			}, nil
		},
	}
	env := newRacingTestEnv(t, fakere)
	dir := env.dir

	// The local command sleeps so that the remote racer reliably wins,
	// then writes the expected content when run as the fallback.
	cmd := &execute.Cmd{
		Args:          []string{"/bin/sh", "-c", "sleep 2; printf 'stubs content' > stubs.jar"},
		Outputs:       []string{"stubs.jar"},
		WorkspaceRoot: dir,
		HashFS:        env.hashFS,
		Platform:      map[string]string{"container-image": "docker://ubuntu"},
		Pure:          true,
		CmdHash:       []byte("cmdhash"),
	}
	cmd.InitOutputs()

	b := env.newBuilder(t, nil)
	step := newRacingTestStep(cmd, []string{"stubs.jar"})

	err := b.runRacing(ctx, step)
	if err != nil {
		t.Fatalf("runRacing=%v; want nil err (local fallback)", err)
	}
	if got, want := step.metrics.RacingWinner, "remote"; got != want {
		t.Errorf("RacingWinner=%q; want %q", got, want)
	}
	if !step.metrics.Fallback {
		t.Errorf("metrics.Fallback=false; want true")
	}
	got, err := os.ReadFile(filepath.Join(dir, "stubs.jar"))
	if err != nil {
		t.Fatalf("stubs.jar not generated by local fallback: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("stubs.jar content=%q; want %q", got, content)
	}
}

// TestRunRacing_RemoteWinMissingOutputFallsBackToLocal checks that when
// the remote racer wins but its action result omits a declared output
// (REAPI servers only return outputs the action produced), runRacing
// falls back to local execution like runRemote does, instead of failing
// the step with "missing outputs". The omitted output is neither in
// hashfs nor on disk, so only a local re-run can produce it.
func TestRunRacing_RemoteWinMissingOutputFallsBackToLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	ctx := t.Context()
	content := []byte("stubs content")
	content2 := []byte("stubs2 content")
	// The remote action result contains only stubs2.jar; stubs.jar is
	// omitted (at least one output must be present so that
	// validateRemoteActionResult doesn't trigger the b/350360391 retry).
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			dg, err := fakere.Put(ctx, content2)
			if err != nil {
				return nil, err
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "stubs2.jar",
						Digest: dg,
					},
				},
			}, nil
		},
	}
	env := newRacingTestEnv(t, fakere)
	dir := env.dir

	// The local command sleeps so that the remote racer reliably wins,
	// then writes both outputs when run as the fallback.
	cmd := &execute.Cmd{
		Args:          []string{"/bin/sh", "-c", "sleep 2; printf 'stubs content' > stubs.jar; printf 'stubs2 content' > stubs2.jar"},
		Outputs:       []string{"stubs.jar", "stubs2.jar"},
		WorkspaceRoot: dir,
		HashFS:        env.hashFS,
		Platform:      map[string]string{"container-image": "docker://ubuntu"},
		Pure:          true,
		CmdHash:       []byte("cmdhash"),
	}
	cmd.InitOutputs()

	b := env.newBuilder(t, nil)
	step := newRacingTestStep(cmd, []string{"stubs.jar", "stubs2.jar"})

	err := b.runRacing(ctx, step)
	if err != nil {
		t.Fatalf("runRacing=%v; want nil err (local fallback)", err)
	}
	if got, want := step.metrics.RacingWinner, "remote"; got != want {
		t.Errorf("RacingWinner=%q; want %q", got, want)
	}
	if !step.metrics.Fallback {
		t.Errorf("metrics.Fallback=false; want true")
	}
	got, err := os.ReadFile(filepath.Join(dir, "stubs.jar"))
	if err != nil {
		t.Fatalf("stubs.jar not generated by local fallback: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("stubs.jar content=%q; want %q", got, content)
	}
	got, err = os.ReadFile(filepath.Join(dir, "stubs2.jar"))
	if err != nil {
		t.Fatalf("stubs2.jar not generated by local fallback: %v", err)
	}
	if string(got) != string(content2) {
		t.Errorf("stubs2.jar content=%q; want %q", got, content2)
	}
}
