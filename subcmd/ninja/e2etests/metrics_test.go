// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_Metrics(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	const queueDuration = 5 * time.Second
	const execDuration = 2 * time.Second
	const fetchDuration = 1 * time.Second
	const uploadDuration = 3 * time.Second
	const workerDuration = fetchDuration + execDuration + uploadDuration

	endTime := time.Now()
	startTime := endTime.Add(-execDuration)
	workerStart := startTime.Add(-fetchDuration)
	workerEnd := endTime.Add(uploadDuration)
	queuedTime := workerStart.Add(-queueDuration)

	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			od, err := fakere.Put(ctx, []byte("remote content"))
			if err != nil {
				return nil, err
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "remote",
						Digest: od,
					},
				},
				ExecutionMetadata: &rpb.ExecutedActionMetadata{
					Worker:                         "fake-worker",
					QueuedTimestamp:                timestamppb.New(queuedTime),
					WorkerStartTimestamp:           timestamppb.New(workerStart),
					InputFetchStartTimestamp:       timestamppb.New(workerStart),
					InputFetchCompletedTimestamp:   timestamppb.New(startTime),
					ExecutionStartTimestamp:        timestamppb.New(startTime),
					ExecutionCompletedTimestamp:    timestamppb.New(endTime),
					OutputUploadStartTimestamp:     timestamppb.New(endTime),
					OutputUploadCompletedTimestamp: timestamppb.New(workerEnd),
					WorkerCompletedTimestamp:       timestamppb.New(workerEnd),
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

	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile:  ".siso_fs_state",
		DataSource: ds,
	})
	defer cleanup()

	opt.REAPIClient = ds.Client
	opt.RECacheEnableRead = true
	opt.RECacheEnableWrite = false
	opt.OutputLocal = func(context.Context, string) bool { return true }

	var metricsBuffer syncBuffer
	opt.MetricsJSONWriter = &metricsBuffer

	t.Logf("-- first build (should be remote execution)")
	_, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
	for dec.More() {
		var m build.StepMetric
		err := dec.Decode(&m)
		if err != nil {
			t.Errorf("decode %v", err)
		}
		if m.StepID == "" {
			continue
		}
		switch filepath.Base(m.Output()) {
		case "local":
			if !m.IsLocal {
				t.Errorf("%s is_local=%t; want true", m.Output(), m.IsLocal)
			}
			if m.IsRemote {
				t.Errorf("%s is_remote=%t; want false", m.Output(), m.IsRemote)
			}
			if m.WorkerTime != 0 {
				t.Errorf("%s worker_time=%v; want 0", m.Output(), m.WorkerTime)
			}
			if m.InputFetchTime != 0 {
				t.Errorf("%s input_fetch=%v; want 0", m.Output(), m.InputFetchTime)
			}
			if m.OutputUploadTime != 0 {
				t.Errorf("%s output_upload=%v; want 0", m.Output(), m.OutputUploadTime)
			}
			if m.ExecTime <= 0 {
				t.Errorf("%s exec=%v; want positive", m.Output(), m.ExecTime)
			}
		case "remote":
			if m.IsLocal {
				t.Errorf("%s is_local=%t; want false", m.Output(), m.IsLocal)
			}
			if !m.IsRemote {
				t.Errorf("%s is_remote=%t; want true", m.Output(), m.IsRemote)
			}
			if time.Duration(m.WorkerTime) != workerDuration {
				t.Errorf("%s worker_time=%v; want %v", m.Output(), m.WorkerTime, workerDuration)
			}
			if time.Duration(m.QueueTime) != queueDuration {
				t.Errorf("%s queue_time=%v; want %v", m.Output(), m.QueueTime, queueDuration)
			}
			if time.Duration(m.ExecTime) != execDuration {
				t.Errorf("%s exec=%v; want %v", m.Output(), m.ExecTime, execDuration)
			}
			if time.Duration(m.InputFetchTime) != fetchDuration {
				t.Errorf("%s input_fetch=%v; want %v", m.Output(), m.InputFetchTime, fetchDuration)
			}
			if time.Duration(m.OutputUploadTime) != uploadDuration {
				t.Errorf("%s output_upload=%v; want %v", m.Output(), m.OutputUploadTime, uploadDuration)
			}
		}
	}

	// Delete local output to force rebuild.
	err = os.Remove(filepath.Join(dir, "out/siso/remote"))
	if err != nil {
		t.Fatal(err)
	}

	// Clear metrics buffer for second run.
	metricsBuffer.buf.Reset()

	t.Logf("-- second build (should be cache hit)")
	_, err = ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}

	dec = json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
	for dec.More() {
		var m build.StepMetric
		err := dec.Decode(&m)
		if err != nil {
			t.Errorf("decode %v", err)
		}
		if m.StepID == "" {
			continue
		}
		if filepath.Base(m.Output()) == "remote" {
			if !m.Cached {
				t.Errorf("remote cached=%t; want true", m.Cached)
			}
			// Verify WorkerTime is omitted (0) for cache hit
			if m.WorkerTime != 0 {
				t.Errorf("remote worker_time=%v; want 0 for cache hit", m.WorkerTime)
			}
			// Verify QueueTime is omitted (0) for cache hit
			if m.QueueTime != 0 {
				t.Errorf("remote queue_time=%v; want 0 for cache hit", m.QueueTime)
			}
			// Verify ExecTime is omitted (0) for cache hit
			if m.ExecTime != 0 {
				t.Errorf("remote exec_time=%v; want 0 for cache hit", m.ExecTime)
			}
			// Verify InputFetchTime is omitted (0) for cache hit
			if m.InputFetchTime != 0 {
				t.Errorf("remote input_fetch=%v; want 0 for cache hit", m.InputFetchTime)
			}
			// Verify OutputUploadTime is omitted (0) for cache hit
			if m.OutputUploadTime != 0 {
				t.Errorf("remote output_upload=%v; want 0 for cache hit", m.OutputUploadTime)
			}
		}
	}
}
