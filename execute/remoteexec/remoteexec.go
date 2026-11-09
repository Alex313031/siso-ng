// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package remoteexec executes cmd with remote exec API.
package remoteexec

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
	_ "go.chromium.org/build/siso/reapi/proto" // for auxiliary metadata
	"go.chromium.org/build/siso/sync/semaphore"
)

// Semaphore enforces a limit on parallel digest calculations to prevent an OOM.
var Semaphore = semaphore.New("remoteexec-digest", runtime.GOMAXPROCS(0)*10)

// RemoteExec is executor with remote exec API.
type RemoteExec struct {
	client *reapi.Client
}

// New creates new remote executor.
func New(ctx context.Context, client *reapi.Client) *RemoteExec {
	return &RemoteExec{
		client: client,
	}
}

func (re *RemoteExec) prepareInputs(ctx context.Context, cmd *execute.Cmd) (digest.Digest, error) {
	var actionDigest digest.Digest
	err := Semaphore.Do(ctx, func(ctx context.Context) error {
		var err error
		ds := blob.NewStore()
		actionDigest, err = cmd.Digest(ctx, ds)
		if err != nil {
			return err
		}
		n, err := re.client.UploadAll(ctx, ds)
		if err != nil {
			return fmt.Errorf("failed to upload all %s: %w", cmd, err)
		}
		clog.Infof(ctx, "upload %d/%d", n, ds.Size())
		return nil
	})
	return actionDigest, err
}

// Execute runs the remote command and waits for completion.
// It does not download outputs or stdout/stderr.
// It returns the ActionResult, whether it was cached, and any execution error.
//
// Canceling ctx stops waiting for CAS input uploads, but does not cancel
// the uploads themselves: they run as upload operations owned by the
// reapi client, shared with other commands that need the same blobs.
func (re *RemoteExec) Execute(ctx context.Context, cmd *execute.Cmd) (*rpb.ActionResult, bool, error) {
	ctx, span := trace.NewSpan(ctx, "remote-exec")
	defer span.Close(nil)
	actionDigest, err := re.prepareInputs(ctx, cmd)
	if err != nil {
		return nil, false, err
	}

	cctx, cspan := trace.NewSpan(ctx, "execute-and-wait")
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeoutCause(cctx, cmd.Timeout, fmt.Errorf("remote exec timeout=%v: %w", cmd.Timeout, context.DeadlineExceeded))
		defer cancel()
	}
	opName, resp, err := re.client.ExecuteAndWait(cctx, &rpb.ExecuteRequest{
		ActionDigest:    actionDigest.Proto(),
		SkipCacheLookup: cmd.SkipCacheLookup,
		DigestFunction:  re.client.DigestFunction().Value(),
	})
	cspan.Close(nil)
	clog.Infof(ctx, "digest: %s, skipCacheLookup:%t opName: %s", actionDigest, cmd.SkipCacheLookup, opName)
	if log.V(1) {
		clog.Infof(ctx, "response: %s", resp)
	}
	if err != nil {
		clog.Warningf(ctx, "digest: %s, err: %v", actionDigest, err)
	}
	result := resp.GetResult()
	re.recordExecuteMetadata(ctx, result, resp.GetCachedResult(), span)
	if result != nil {
		cmd.SetActionResult(result, resp.GetCachedResult())
	}
	return result, resp.GetCachedResult(), err
}

// Run runs a cmd.
func (re *RemoteExec) Run(ctx context.Context, cmd *execute.Cmd) error {
	result, cached, err := re.Execute(ctx, cmd)
	return re.ProcessResult(ctx, cmd, result, cached, err)
}

func (re *RemoteExec) recordExecuteMetadata(ctx context.Context, result *rpb.ActionResult, cached bool, span *trace.Span) {
	md := result.GetExecutionMetadata()
	queue := trace.SpanData{
		Name:  "rbe:queue",
		Start: md.GetQueuedTimestamp().AsTime(),
		End:   md.GetWorkerStartTimestamp().AsTime(),
	}
	if cached {
		span.SetAttr("rbe:queue", queue.Duration())
	} else {
		span.Add(ctx, queue)
	}
	worker := trace.SpanData{
		Name:  "rbe:worker",
		Start: md.GetWorkerStartTimestamp().AsTime(),
		End:   md.GetWorkerCompletedTimestamp().AsTime(),
		Attrs: map[string]any{
			"worker": md.GetWorker(),
		},
	}
	var wspan *trace.Span
	if cached {
		span.SetAttr("rbe:worker", worker.Duration())
	} else {
		wspan = span.Add(ctx, worker)
	}
	input := trace.SpanData{
		Name:  "rbe:input",
		Start: md.GetInputFetchStartTimestamp().AsTime(),
		End:   md.GetInputFetchCompletedTimestamp().AsTime(),
	}
	if cached {
		span.SetAttr("rbe:input", input.Duration())
	} else {
		wspan.Add(ctx, input)
	}
	exec := trace.SpanData{
		Name:  "rbe:exec",
		Start: md.GetExecutionStartTimestamp().AsTime(),
		End:   md.GetExecutionCompletedTimestamp().AsTime(),
	}
	if cached {
		span.SetAttr("rbe:exec", exec.Duration())
	} else {
		wspan.Add(ctx, exec)
	}
	output := trace.SpanData{
		Name:  "rbe:output",
		Start: md.GetOutputUploadStartTimestamp().AsTime(),
		End:   md.GetOutputUploadCompletedTimestamp().AsTime(),
	}
	if cached {
		span.SetAttr("rbe:output", output.Duration())
	} else {
		wspan.Add(ctx, output)
	}
	clog.Infof(ctx, "execution metadata: %s queue=%s worker=%s input=%s exec=%s output=%s", md.GetWorker(), queue.Duration(), worker.Duration(), input.Duration(), exec.Duration(), output.Duration())
	for _, aux := range md.GetAuxiliaryMetadata() {
		amd, err := aux.UnmarshalNew()
		if err != nil {
			clog.Warningf(ctx, "unknown aux metadata %s: %v", aux.GetTypeUrl(), err)
			continue
		}
		clog.Infof(ctx, "execution auxiliary metadata %T: %s", amd, amd)
	}
}

// ProcessResult downloads the results from the remote step (outputs, stdout, stderr) and places them.
func (re *RemoteExec) ProcessResult(ctx context.Context, cmd *execute.Cmd, result *rpb.ActionResult, cached bool, err error) error {
	if result.GetExitCode() == 0 && err == nil {
		clog.Infof(ctx, "exit=%d cache=%t", result.GetExitCode(), cached)
	} else {
		clog.Warningf(ctx, "exit=%d cache=%t result=%v err:%v", result.GetExitCode(), cached, result, err)
	}
	if result == nil {
		if err != nil {
			return err
		}
		return errors.New("no result")
	}
	now := time.Now()

	// Record auxiliary outputs logs (e.g. crash reports) for debugging.
	cmd.RecordAuxiliaryOutputDigests(ctx, result)

	// Leave the raw result as-is: a dir output's Tree is flattened when the
	// result is recorded (cmd.RecordOutputs -> expandDirOutputs), the single
	// path every result flows through. Expanding here would duplicate that.
	cmd.SetActionResult(result, cached)
	if len(result.GetStdoutRaw()) > 0 {
		cmd.StdoutWriter().Write(result.GetStdoutRaw())
	} else {
		stdoutErr := setStdout(ctx, re.client, result.GetStdoutDigest(), cmd)
		if err == nil {
			err = stdoutErr
		}
	}
	if len(result.GetStderrRaw()) > 0 {
		cmd.StderrWriter().Write(result.GetStderrRaw())
	} else {
		stderrErr := setStderr(ctx, re.client, result.GetStderrDigest(), cmd)
		if err == nil {
			err = stderrErr
		}
	}
	if err != nil {
		// Even when err was not nil, the outputs/stdout/stderr were populated to ActionResult for investigation.
		return err
	}
	cmdErr := resultErr(result)
	if cmdErr != nil {
		return cmdErr
	}
	// update output file only step succeeded.
	if cmd.SkipRecordOutputs {
		return nil
	}
	// hashfs may hold a stale local-ready entry for an output that was
	// removed from disk behind Siso's back after .siso_fs_state was loaded.
	// If the remote result has the same digest, the stale entry would be
	// kept as-is, and with restat_content also the same mtime, so Flush
	// would silently skip re-materializing the missing file. Forget the
	// outputs so that re-recording them gives the entries a CAS-backed
	// source and Flush verifies them against the local disk.
	cmd.HashFS.ForgetOutputs(ctx, cmd.WorkspaceRoot, cmd.AllOutputs())
	return cmd.RecordOutputs(ctx, cmd.HashFS.DataSource(), now)
}

// share it with localexec?

func resultErr(result *rpb.ActionResult) error {
	if result == nil {
		return errors.New("no result")
	}
	if result.GetExitCode() == 0 {
		return nil
	}
	return execute.ExitError{
		ExitCode: int(result.GetExitCode()),
	}
}

func setStdout(ctx context.Context, client *reapi.Client, d *rpb.Digest, cmd *execute.Cmd) error {
	if d == nil || d.SizeBytes == 0 {
		return nil
	}
	w := cmd.StdoutWriter()
	b, err := client.Get(ctx, digest.FromProto(d), "stdout")
	if err != nil {
		return err
	}
	w.Write(b)
	return nil
}

func setStderr(ctx context.Context, client *reapi.Client, d *rpb.Digest, cmd *execute.Cmd) error {
	if d == nil || d.SizeBytes == 0 {
		return nil
	}
	w := cmd.StderrWriter()
	b, err := client.Get(ctx, digest.FromProto(d), "stderr")
	if err != nil {
		return err
	}
	w.Write(b)
	return nil
}
