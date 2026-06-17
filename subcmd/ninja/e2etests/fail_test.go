// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"strings"
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
	"go.chromium.org/build/siso/ui"
)

func TestBuild_Fail_Remote(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, refake *reapitest.Fake, failureSummary, outputLog *bytes.Buffer) (build.Stats, error) {
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
		opt.FailureSummaryWriter = failureSummary
		opt.OutputLogWriter = outputLog
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("first build")
	setupFiles(t, dir, t.Name(), nil)
	fakereSuccess := &reapitest.Fake{
		ExecuteFunc: func(re *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			t.Logf("remote succeed")
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "gen/foo.srcjar",
						Digest: digest.Empty.Proto(),
					},
				},
			}, nil
		},
	}
	var failureSummary, outputLog bytes.Buffer
	stats, err := runNinjaTest(t, fakereSuccess, &failureSummary, &outputLog)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != 3 || stats.NoExec != 1 || stats.Remote != 1 || stats.Skipped != 1 {
		t.Fatalf("ninja stats done=%d NoExec=%d Remote=%d Skipped=%d; want done=3 NoExec=1 Remote=1 Skipped=1", stats.Done, stats.NoExec, stats.Remote, stats.Skipped)
	}
	if len(failureSummary.Bytes()) != 0 {
		t.Errorf("ninja failure=%q; want empty", failureSummary.String())
	}
	failureSummary.Reset()
	if len(outputLog.Bytes()) != 0 {
		t.Errorf("ninja output_log=%q; want empty", outputLog.String())
	}
	outputLog.Reset()

	t.Logf("first confirm no-op")
	stats, err = runNinjaTest(t, fakereSuccess, &failureSummary, &outputLog)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != 3 || stats.Skipped != 3 || stats.Remote != 0 || stats.Local != 0 || stats.NoExec != 0 {
		t.Fatalf("ninja confirm no-op error? stats=%#v", stats)
	}
	if len(failureSummary.Bytes()) != 0 {
		t.Errorf("ninja failure=%q; want empty", failureSummary.String())
	}
	failureSummary.Reset()
	if len(outputLog.Bytes()) != 0 {
		t.Errorf("ninja output_log=%q; want empty", outputLog.String())
	}
	outputLog.Reset()

	t.Logf("-- make bad foo.txt and fail gen/foo.srcjar")
	modifyFile(t, dir, "foo.txt", func([]byte) []byte {
		return []byte("error")
	})

	fakereErr := &reapitest.Fake{
		ExecuteFunc: func(re *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			t.Logf("remote fail")
			return &rpb.ActionResult{
				ExitCode:  1,
				StderrRaw: []byte("reapi error"),
			}, nil
		},
	}
	var stdout, stderr syncBuffer
	ui.Default = ui.LogUI{
		Stdout: &stdout,
		Stderr: &stderr,
	}
	t.Cleanup(func() {
		ui.Default = ui.LogUI{}
	})
	stats, err = runNinjaTest(t, fakereErr, &failureSummary, &outputLog)
	if err == nil {
		t.Fatalf("ninja succeeded, but want err; stats=%#v", stats)
	}
	// no fail fallback, so remote=1 local=0 fail=1, not remote=0 local=1 fail=1
	if stats.Done != 1 || stats.Fail != 1 || stats.Remote != 1 || stats.Local != 0 {
		t.Fatalf("ninja stats done=%d Fail=%d Remote=%d Local=%d; want done=1 Fail=1 Remote=1 Local=0 %#v", stats.Done, stats.Fail, stats.Remote, stats.Local, stats)
	}
	if len(failureSummary.Bytes()) == 0 {
		t.Errorf("ninja failure=%q; want empty (fallback)", failureSummary.String())
	}
	failureSummary.Reset()
	if !strings.Contains(outputLog.String(), "reapi error") {
		t.Errorf("ninja output_log=%q; want 'reapi error'", outputLog.String())
	}
	outputLog.Reset()
	outString := stdout.String() + stderr.String()
	if !strings.Contains(outString, "FAILED:") || !strings.Contains(outString, "reapi error") {
		t.Errorf("ninja output missing `FAILED:` or `reapi error`\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	t.Logf("rerun ninja, should fail again")
	stats, err = runNinjaTest(t, fakereErr, &failureSummary, &outputLog)
	if err == nil {
		t.Fatalf("ninja succeeded, but want err; stats=%#v", stats)
	}
	if stats.Done != 1 || stats.Fail != 1 || stats.Remote != 1 || stats.Local != 0 {
		t.Fatalf("ninja stats done=%d Fail=%d Remote=%d Local=%d; want done=1 Fail=1 Remote=1 Local=0", stats.Done, stats.Fail, stats.Remote, stats.Local)
	}
	if len(failureSummary.Bytes()) == 0 {
		t.Errorf("ninja failure=%q; want empty (fallback)", failureSummary.String())
	}
	failureSummary.Reset()
	if !strings.Contains(outputLog.String(), "reapi error") {
		t.Errorf("ninja output_log=%q; want 'reapi error'", outputLog.String())
	}
	outputLog.Reset()

	t.Logf("-- fix foo.txt")
	modifyFile(t, dir, "foo.txt", func([]byte) []byte {
		return []byte("ok")
	})

	stats, err = runNinjaTest(t, fakereSuccess, &failureSummary, &outputLog)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != 3 || stats.NoExec != 1 || stats.Remote != 1 || stats.Skipped != 1 {
		t.Fatalf("ninja stats done=%d NoExec=%d Remote=%d Skipped=%d; want done=3 NoExec=1 Remote=1 Skipped=1", stats.Done, stats.NoExec, stats.Remote, stats.Skipped)
	}
	if len(failureSummary.Bytes()) != 0 {
		t.Errorf("ninja failure=%q; want empty", failureSummary.String())
	}
	failureSummary.Reset()
	if len(outputLog.Bytes()) != 0 {
		t.Errorf("ninja output_log=%q; want empty", outputLog.String())
	}
	outputLog.Reset()
}
