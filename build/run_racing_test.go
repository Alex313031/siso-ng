// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi"
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
			name: "context_canceled",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "wrapped_context_canceled",
			err:  fmt.Errorf("something: %w", context.Canceled),
			want: true,
		},
		{
			name: "grpc_canceled_status",
			err:  status.Error(codes.Canceled, "context canceled"),
			want: true,
		},
		{
			name: "wrapped_grpc_canceled_status",
			err:  fmt.Errorf("find missing: %w", status.Error(codes.Canceled, "context canceled")),
			want: true,
		},
		{
			name: "deeply_wrapped_grpc_canceled_cas_upload_path",
			err: fmt.Errorf("failed to upload all foo: %w",
				fmt.Errorf("wait for digest=abc/123: %w",
					fmt.Errorf("find missing: %w",
						status.Error(codes.Canceled, "context canceled")))),
			want: true,
		},
		{
			name: "grpc_deadline_exceeded_status",
			err:  status.Error(codes.DeadlineExceeded, "deadline exceeded"),
			want: false,
		},
		{
			name: "grpc_unavailable_status",
			err:  status.Error(codes.Unavailable, "unavailable"),
			want: false,
		},
		{
			name: "unrelated_error",
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
			cmdStdout:          "compile error",
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if exitErr, ok := errors.AsType[execute.ExitError](err); !ok {
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
				wantErr := status.Error(codes.Unavailable, "unavailable")
				if fallbackErr, ok := errors.AsType[TooManyFallbackError](err); !ok {
					t.Errorf("got %T: %v, expected TooManyFallbackError", err, err)
				} else if !errors.Is(fallbackErr.Err, wantErr) {
					t.Errorf("got %v, expected wrapped error %v", fallbackErr.Err, wantErr)
				}
			},
		},
		{
			name:               "DepsError_NoFallback",
			err:                DepsError{Err: errors.New("missing header")},
			maxFallbackAllowed: 10,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if _, ok := errors.AsType[DepsError](err); !ok {
					t.Errorf("got %T: %v, expected DepsError", err, err)
				}
				if _, ok := errors.AsType[TooManyFallbackError](err); ok {
					t.Errorf("unexpected TooManyFallbackError for DepsError: %v", err)
				}
			},
		},
		{
			name:               "DepsError_UnsandboxedInputs_NoFallback",
			err:                DepsError{UnsandboxedInputs: []string{"../../undeclared.h"}},
			maxFallbackAllowed: 10,
			wantOk:             false,
			checkErr: func(t *testing.T, err error) {
				if _, ok := errors.AsType[DepsError](err); !ok {
					t.Errorf("got %T: %v, expected DepsError", err, err)
				}
				if _, ok := errors.AsType[TooManyFallbackError](err); ok {
					t.Errorf("unexpected TooManyFallbackError for DepsError: %v", err)
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
	cachestore *reapi.LocalCache
	hashFS     *hashfs.HashFS
}

// newRacingTestEnv creates the env; dialOpts are forwarded to the fake
// REAPI client connection (e.g. to install a reapitest.RPCGate).
func newRacingTestEnv(t *testing.T, fakere *reapitest.Fake, dialOpts ...grpc.DialOption) *racingTestEnv {
	t.Helper()
	ctx := t.Context()
	// t.Context() is canceled before t.Cleanup functions run.
	cleanupCtx := context.WithoutCancel(ctx)
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	reclient := reapitest.NewWithOption(ctx, t, fakere, reapi.Option{}, dialOpts...)
	cachestore, err := reapi.NewLocalCache(filepath.Join(dir, ".siso_cache"))
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
			outputs:    path.Paths(outputs),
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
		Outputs:       []path.Path{"stubs.jar"},
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
				Data: blob.FromBytes(digest.SHA256, "stubs.jar", content),
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
	if !bytes.Equal(got, content) {
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
	missing := blob.FromBytes(digest.SHA256, "stubs.jar", content).Digest()
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
		Outputs:       []path.Path{"stubs.jar"},
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
	if !bytes.Equal(got, content) {
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
		Outputs:       []path.Path{"stubs.jar", "stubs2.jar"},
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

// TestRunRacing_LocalWinDoesNotWaitForRemoteUpload checks that when the
// local racer wins while the remote racer is still uploading inputs to
// the CAS, runRacing returns as soon as the local side finishes instead
// of blocking until the upload completes. b/565765743
//
// The remote racer's BatchUpdateBlobs is held by an RPC gate for the
// whole test; the local command finishes immediately.
func TestRunRacing_LocalWinDoesNotWaitForRemoteUpload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	ctx := t.Context()
	content := []byte("stubs content")
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			t.Error("remote execution must not start: inputs never finish uploading")
			return nil, status.Error(codes.Internal, "unexpected remote execution")
		},
	}
	gate := reapitest.NewRPCGate(rpb.ContentAddressableStorage_BatchUpdateBlobs_FullMethodName)
	t.Cleanup(gate.Release)
	env := newRacingTestEnv(t, fakere, gate.DialOption())
	dir := env.dir

	cmd := &execute.Cmd{
		Args:          []string{"/bin/sh", "-c", "printf 'stubs content' > stubs.jar"},
		Outputs:       []path.Path{"stubs.jar"},
		WorkspaceRoot: dir,
		HashFS:        env.hashFS,
		Platform:      map[string]string{"container-image": "docker://ubuntu"},
		Pure:          true,
		CmdHash:       []byte("cmdhash"),
	}
	cmd.InitOutputs()

	b := env.newBuilder(t, nil)
	step := newRacingTestStep(cmd, []string{"stubs.jar"})

	done := make(chan error, 1)
	go func() { done <- b.runRacing(ctx, step) }()
	// Make sure the remote racer really is stuck in the upload while
	// the local racer runs. The gate stays held until cleanup, so
	// runRacing can only return here if the local win did not wait for
	// the upload; on the previous code this receive hangs (bounded by
	// `go test -timeout`).
	gate.WaitEntered(ctx, t)
	if err := <-done; err != nil {
		t.Fatalf("runRacing=%v; want nil", err)
	}
	if got, want := step.metrics.RacingWinner, "local"; got != want {
		t.Errorf("RacingWinner=%q; want %q", got, want)
	}
	got, err := os.ReadFile(filepath.Join(dir, "stubs.jar"))
	if err != nil {
		t.Fatalf("stubs.jar not generated by local: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("stubs.jar content=%q; want %q", got, content)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) {
	return f(p)
}

// TestRunRacing_RemoteFailureWithLocalWin checks fallback accounting when the
// remote racer fails with a fallbackable error (ExitCode 137) and the local
// racer succeeds:
//   - local_wins_first: local finishes before remote fails; must be recorded
//     as a clean local win without claiming fallback.
//   - remote_fails_first: remote fails while local is still running; must
//     claim fallback and wait for the local racer to finish.
func TestRunRacing_RemoteFailureWithLocalWin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	for _, tc := range []struct {
		name          string
		remoteFirst   bool
		wantFallback  bool
		wantFallbacks int64
	}{
		{
			name:          "local_wins_first",
			remoteFirst:   false,
			wantFallback:  false,
			wantFallbacks: 0,
		},
		{
			name:          "remote_fails_first",
			remoteFirst:   true,
			wantFallback:  true,
			wantFallbacks: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			content := []byte("output content")

			var releaseLocal func()
			raceCanceled := make(chan struct{})
			fakere := &reapitest.Fake{
				ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
					if !tc.remoteFirst {
						// Unblock the local racer once remote execution has started,
						// then wait for the local racer to win and trigger raceCancel()
						// before returning the remote failure.
						releaseLocal()
						<-raceCanceled
					}
					// ExitCode 137 (SIGKILL/OOM) fails immediately without REAPI
					// RPC retries and is eligible for local fallback.
					return &rpb.ActionResult{ExitCode: 137}, nil
				},
			}
			// In the local_wins_first case, raceCancel() would normally abort the
			// gRPC Execute stream with codes.Canceled before the fake server's
			// ExitCode 137 response is received. Observe raceCancel() on ctx via
			// AfterFunc, but strip cancellation from the stream context so the
			// remote goroutine receives ExitCode 137 instead of context.Canceled.
			streamOpt := grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				if !tc.remoteFirst && method == rpb.Execution_Execute_FullMethodName {
					context.AfterFunc(ctx, func() { close(raceCanceled) })
					ctx = context.WithoutCancel(ctx)
				}
				return streamer(ctx, desc, cc, method, opts...)
			})
			env := newRacingTestEnv(t, fakere, streamOpt)
			b := env.newBuilder(t, func(opts *Options) {
				if tc.remoteFirst {
					// b.setupFallback synchronously writes a FALLBACK entry to
					// OutputLogWriter; use that write to unblock the local racer
					// only after remote failure has been processed as a fallback.
					opts.OutputLogWriter = writerFunc(func(p []byte) (int, error) {
						releaseLocal()
						return len(p), nil
					})
				}
			})
			// Pre-acquire the single local semaphore slot (Limits.Local = 1) so
			// the local racer blocks until releaseLocal() is called at the exact
			// point required by each test case.
			_, done, err := b.localSema.WaitAcquire(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			releaseLocal = sync.OnceFunc(func() { done(nil) })

			cmd := &execute.Cmd{
				Args:          []string{"/bin/sh", "-c", "printf 'output content' > stubs.jar"},
				Outputs:       []path.Path{"stubs.jar"},
				WorkspaceRoot: env.dir,
				HashFS:        env.hashFS,
				Platform:      map[string]string{"container-image": "docker://ubuntu"},
				Pure:          true,
				CmdHash:       []byte("cmdhash"),
			}
			cmd.InitOutputs()
			step := newRacingTestStep(cmd, []string{"stubs.jar"})

			if err := b.runRacing(ctx, step); err != nil {
				t.Fatalf("runRacing=%v; want nil err", err)
			}
			if got, want := step.metrics.RacingWinner, "local"; got != want {
				t.Errorf("RacingWinner=%q; want %q", got, want)
			}
			if got := step.metrics.Fallback; got != tc.wantFallback {
				t.Errorf("metrics.Fallback=%t; want %t", got, tc.wantFallback)
			}
			if got := b.numFallback.Load(); got != tc.wantFallbacks {
				t.Errorf("b.numFallback=%d; want %d", got, tc.wantFallbacks)
			}
			got, err := os.ReadFile(filepath.Join(env.dir, "stubs.jar"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) {
				t.Errorf("stubs.jar content=%q; want %q", got, content)
			}
		})
	}
}

// waitForLocalKilledLog returns a context whose clog logger closes ch once
// runRacing logs "racing: local killed".
func waitForLocalKilledLog(ctx context.Context) (context.Context, <-chan struct{}) {
	ch := make(chan struct{})
	var once sync.Once
	w := writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "racing: local killed") {
			once.Do(func() { close(ch) })
		}
		return len(p), nil
	})
	return clog.NewContext(ctx, clog.FromContext(ctx).WithWriter(w)), ch
}

// remoteSucceedsAfter returns a fake remote execution that waits until ready
// is closed, then finishes with stubs.jar containing content.
func remoteSucceedsAfter(ctx context.Context, ready <-chan struct{}, content []byte) func(*reapitest.Fake, *rpb.Action) (*rpb.ActionResult, error) {
	return func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
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
	}
}

// remoteFailsAfter returns a fake remote execution that waits until ready is
// closed, then fails with exitCode.
func remoteFailsAfter(ctx context.Context, ready <-chan struct{}, exitCode int32) func(*reapitest.Fake, *rpb.Action) (*rpb.ActionResult, error) {
	return func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &rpb.ActionResult{ExitCode: exitCode}, nil
	}
}

// newShRacingCmd returns a pure racing test command that runs script with
// /bin/sh in the env's workspace and declares stubs.jar as its output.
func newShRacingCmd(env *racingTestEnv, script string) *execute.Cmd {
	cmd := &execute.Cmd{
		Args:          []string{"/bin/sh", "-c", script},
		Outputs:       []path.Path{"stubs.jar"},
		WorkspaceRoot: env.dir,
		HashFS:        env.hashFS,
		Platform:      map[string]string{"container-image": "docker://ubuntu"},
		Pure:          true,
		CmdHash:       []byte("cmdhash"),
	}
	cmd.InitOutputs()
	return cmd
}

// TestRunRacing_LocalKilledBySignal_RemoteWins checks that when the local
// racer is killed by a signal (e.g. by the kernel OOM killer) while the
// remote racer is still running, runRacing waits for the remote result
// instead of canceling it and failing the step. b/571235311
func TestRunRacing_LocalKilledBySignal_RemoteWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	for _, tc := range []struct {
		name   string
		script string
	}{
		{
			// localexec reports exit=-1 for a process killed by a signal.
			name:   "SIGKILL",
			script: "kill -9 $$",
		},
		{
			// Sandbox wrappers such as nsjail exit with 128+SIGKILL.
			name:   "exit137",
			script: "exit 137",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, localKilled := waitForLocalKilledLog(t.Context())
			content := []byte("remote content")
			fakere := &reapitest.Fake{
				// Finish only after runRacing has seen the local racer
				// killed, so that the local racer always finishes first.
				ExecuteFunc: remoteSucceedsAfter(ctx, localKilled, content),
			}
			env := newRacingTestEnv(t, fakere)
			b := env.newBuilder(t, nil)
			step := newRacingTestStep(newShRacingCmd(env, tc.script), []string{"stubs.jar"})

			err := b.runRacing(ctx, step)
			if err != nil {
				t.Fatalf("runRacing=%v; want nil err (remote result after local was killed)", err)
			}
			if got, want := step.metrics.RacingWinner, "remote"; got != want {
				t.Errorf("RacingWinner=%q; want %q", got, want)
			}
			if step.metrics.Fallback {
				t.Errorf("metrics.Fallback=true; want false")
			}
			if got := b.numFallback.Load(); got != 0 {
				t.Errorf("b.numFallback=%d; want 0", got)
			}
			got, err := os.ReadFile(filepath.Join(env.dir, "stubs.jar"))
			if err != nil {
				t.Fatalf("stubs.jar not materialized from remote: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Errorf("stubs.jar content=%q; want %q", got, content)
			}
		})
	}
}

// TestRunRacing_LocalKilledBySignal_RemoteFails_FallsBackToLocal checks that
// when the local racer is killed by a signal and the remote racer then fails
// too, runRacing applies the usual remote failure policy: it claims a
// fallback and runs the step locally once more.
func TestRunRacing_LocalKilledBySignal_RemoteFails_FallsBackToLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	ctx, localKilled := waitForLocalKilledLog(t.Context())
	content := []byte("fallback content")
	fakere := &reapitest.Fake{
		// Fail with exit code 137 (remote OOM, eligible for local
		// fallback) only after runRacing has seen the local racer killed.
		ExecuteFunc: remoteFailsAfter(ctx, localKilled, 137),
	}
	env := newRacingTestEnv(t, fakere)
	// The first run (the local racer) kills itself; the second run (the
	// fallback) writes the output.
	marker := filepath.Join(t.TempDir(), "killed")
	script := fmt.Sprintf("if [ -e %[1]q ]; then printf 'fallback content' > stubs.jar; else touch %[1]q; kill -9 $$; fi", marker)
	b := env.newBuilder(t, nil)
	step := newRacingTestStep(newShRacingCmd(env, script), []string{"stubs.jar"})

	err := b.runRacing(ctx, step)
	if err != nil {
		t.Fatalf("runRacing=%v; want nil err (local fallback)", err)
	}
	if !step.metrics.Fallback {
		t.Errorf("metrics.Fallback=false; want true")
	}
	if got := b.numFallback.Load(); got != 1 {
		t.Errorf("b.numFallback=%d; want 1", got)
	}
	got, err := os.ReadFile(filepath.Join(env.dir, "stubs.jar"))
	if err != nil {
		t.Fatalf("stubs.jar not generated by local fallback: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("stubs.jar content=%q; want %q", got, content)
	}
}

// TestRunRacing_LocalKilledBySignal_RemoteFails_NoFallback checks that when
// the local racer is killed by a signal, the remote racer fails too, and
// fallback is not allowed, the step fails without running the command
// locally again.
func TestRunRacing_LocalKilledBySignal_RemoteFails_NoFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	ctx, localKilled := waitForLocalKilledLog(t.Context())
	fakere := &reapitest.Fake{
		// Fail only after runRacing has seen the local racer killed.
		ExecuteFunc: remoteFailsAfter(ctx, localKilled, 137),
	}
	env := newRacingTestEnv(t, fakere)
	runs := filepath.Join(t.TempDir(), "runs")
	script := fmt.Sprintf("echo run >> %q; kill -9 $$", runs)
	b := env.newBuilder(t, nil)
	// No fallback budget left: the first claim exceeds the limit.
	b.maxFallbackAllowed = 1
	step := newRacingTestStep(newShRacingCmd(env, script), []string{"stubs.jar"})

	err := b.runRacing(ctx, step)
	wantErr := execute.ExitError{ExitCode: 137}
	if fallbackErr, ok := errors.AsType[TooManyFallbackError](err); !ok {
		t.Fatalf("runRacing=%v (%T); want TooManyFallbackError", err, err)
	} else if !errors.Is(fallbackErr.Err, wantErr) {
		t.Fatalf("fallbackErr.Err=%v; want %v", fallbackErr.Err, wantErr)
	}
	if step.metrics.Fallback {
		t.Errorf("metrics.Fallback=true; want false")
	}
	buf, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(buf), "run"); got != 1 {
		t.Errorf("local command ran %d times; want 1", got)
	}
}

// TestRunRacing_LocalFailure_DoesNotWaitForRemote checks that an ordinary
// local failure (e.g. a compile error) still wins the race right away and
// cancels the remote racer, so build errors are reported without waiting
// for the remote result.
func TestRunRacing_LocalFailure_DoesNotWaitForRemote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runs /bin/sh")
	}
	ctx, localKilled := waitForLocalKilledLog(t.Context())
	fakere := &reapitest.Fake{
		// Simulate a slow remote execution that would fail with the same
		// exit code: it doesn't finish before the test ends unless
		// runRacing logs "racing: local killed" and waits for it.
		ExecuteFunc: remoteFailsAfter(ctx, localKilled, 1),
	}
	env := newRacingTestEnv(t, fakere)
	b := env.newBuilder(t, nil)
	step := newRacingTestStep(newShRacingCmd(env, "exit 1"), []string{"stubs.jar"})

	err := b.runRacing(ctx, step)
	eerr, ok := errors.AsType[execute.ExitError](err)
	if !ok || eerr.ExitCode != 1 {
		t.Fatalf("runRacing=%v; want exit=1 from the local racer", err)
	}
	if got, want := step.metrics.RacingWinner, "local"; got != want {
		t.Errorf("RacingWinner=%q; want %q", got, want)
	}
	if step.metrics.Fallback {
		t.Errorf("metrics.Fallback=true; want false")
	}
}
