// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package remoteexec executes cmd with remote exec API.
package remoteexec

import (
	"context"
	"errors"
	"fmt"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
	_ "go.chromium.org/build/siso/reapi/proto" // for auxiliary metadata
	"go.chromium.org/build/siso/runtimex"
	"go.chromium.org/build/siso/sync/semaphore"
)

// Semaphore enforces a limit on parallel digest calculations to prevent an OOM.
var Semaphore = semaphore.New("remoteexec-digest", runtimex.NumCPU()*10)

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
		ds := digest.NewStore()
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

// Run runs a cmd.
func (re *RemoteExec) Run(ctx context.Context, cmd *execute.Cmd) error {
	ctx, span := trace.NewSpan(ctx, "remote-exec")
	defer span.Close(nil)
	actionDigest, err := re.prepareInputs(ctx, cmd)
	if err != nil {
		return err
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
	return re.processResult(ctx, cmd, result, resp.GetCachedResult(), err)
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

func (re *RemoteExec) processResult(ctx context.Context, cmd *execute.Cmd, result *rpb.ActionResult, cached bool, err error) error {
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
	files := result.GetOutputFiles()
	symlinks := result.GetOutputSymlinks()
	var dirs []*rpb.OutputDirectory
	for _, d := range result.GetOutputDirectories() {
		ds := digest.NewStore()
		outdir, derr := re.client.FetchTree(ctx, d.GetPath(), digest.FromProto(d.GetTreeDigest()), ds)
		if derr != nil {
			clog.Errorf(ctx, "Failed to fetch tree %s %s: %v", d.GetPath(), d.GetTreeDigest(), derr)
			continue
		}
		dfiles, dsymlinks, ddirs := merkletree.Traverse(ctx, d.GetPath(), outdir, ds)
		files = append(files, dfiles...)
		symlinks = append(symlinks, dsymlinks...)
		dirs = append(dirs, ddirs...)
	}
	result.OutputFiles = files
	result.OutputSymlinks = symlinks
	result.OutputDirectories = dirs
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
