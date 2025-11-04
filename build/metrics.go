// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	epb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/o11y/clog"
)

// IntervalMetric is a time duration, but serialized as seconds in JSON.
type IntervalMetric time.Duration

// MarshalJSON marshals the IntervalMetric as float64 of seconds.
func (i IntervalMetric) MarshalJSON() ([]byte, error) {
	d := time.Duration(i)
	// Reduce precesion to make siso_metrics.json smaller.
	sec := strconv.FormatFloat(d.Seconds(), 'f', 2, 64)
	return []byte(sec), nil
}

// UnmarshalJSON unmarshals float64 of seconds as an IntervalMetric.
func (i *IntervalMetric) UnmarshalJSON(b []byte) error {
	var secs float64
	err := json.Unmarshal(b, &secs)
	if err != nil {
		return err
	}
	*i = IntervalMetric(time.Duration(int64(secs * 1e9)))
	return nil
}

// StepMetric contains metrics about a build Step.
type StepMetric struct {
	BuildID string `json:"build_id,omitempty"` // the unique id of the current build (trace)
	StepID  string `json:"step_id,omitempty"`  // the unique id of this step (top span)

	Rule     string `json:"rule,omitempty"`      // the rule name of the step
	Action   string `json:"action,omitempty"`    // the action name of the step
	Output   string `json:"output,omitempty"`    // the name of the first output file of the step
	GNTarget string `json:"gn_target,omitempty"` // inferred gn target

	// The ID and name of the first output of the previous step.
	// The "previous" step is defined as the last step that updated
	// the output that is used as part of this step's inputs.
	PrevStepID  string `json:"prev,omitempty"`
	PrevStepOut string `json:"prev_out,omitempty"`

	// Ready, Start and Duration are measured by Siso's scheduler,
	// independently of the measurements provided by the execution
	// strategies (see RunTime, QueueTime, ExecTime below).

	// Ready is the time it took since build start until the action became
	// ready for execution (= all inputs are available).
	Ready IntervalMetric `json:"ready,omitempty"`
	// Start is the time it took until Siso's scheduler was ready to work
	// on the step (concurrency limited by stepSema) and pass it to an
	// execution strategy.
	Start IntervalMetric `json:"start,omitempty"`
	// Duration is the time it took for the action to do its job, measured
	// from start of work until it is completed.
	// It includes siso-overhead (preproc etc) and command executon
	// (RunTime).
	// for full build metric, it's duration to process all scheduled steps.
	Duration IntervalMetric `json:"duration,omitempty"`

	// WeightedDuration is an estimate of the "true duration" of the action
	// that tries to accommodate for the impact of other actions running in
	// parallel. It is calculated by summing up small slices of time (~100ms)
	// while the action is running, where each slice's duration is divided by
	// the number of concurrently running actions at that point in time.
	WeightedDuration IntervalMetric `json:"weighted_duration,omitempty"`

	// The hash of the command-line of the build step.
	CmdHash string `json:"cmdhash,omitempty"`
	// The hash of the action proto of this build step.
	Digest string `json:"digest,omitempty"`

	DepsLog       bool `json:"deps_log,omitempty"`       // whether the action used the deps log.
	DepsLogErr    bool `json:"deps_log_err,omitempty"`   // whether the action failed with deps log.
	ScandepsErr   bool `json:"scandeps_err,omitempty"`   // whether the action failed in scandeps.
	ClangScandeps bool `json:"clang_scandeps,omitempty"` // whether the action used the clang for scandeps.

	NoExec        bool `json:"no_exec,omitempty"`         // whether the action didn't run any command (i.e. just use handler).
	IsRemote      bool `json:"is_remote,omitempty"`       // whether the action uses remote result.
	IsLocal       bool `json:"is_local,omitempty"`        // whether the action uses local result.
	FastLocal     bool `json:"fast_local,omitempty"`      // whether the action chooses local for fast build.
	StartLocal    bool `json:"start_local,omitempty"`     // whether the action chooses local for start in incremental build.
	CacheWrite    bool `json:"cache_write,omitempty"`     // whether the action used cache write feature from local results.
	CacheWriteErr bool `json:"cache_write_err,omitempty"` // whether the action failed while using cache write feature.
	Cached        bool `json:"cached,omitempty"`          // whether the action was a cache hit.
	Fallback      bool `json:"fallback,omitempty"`        // whether the action failed remotely and was retried locally.
	Err           bool `json:"err,omitempty"`             // whether the action failed.
	RemoteRetry   int  `json:"remote_retry,omitempty"`    // count of remote retry

	// DepsScanTime is the time it took in calculating deps for cmd inputs.
	// TODO: set in reproxy mode too
	DepsScanTime IntervalMetric `json:"depsscan,omitempty"`

	// RunTime, QueueTime and ExecTime are measured by the execution
	// strategies in execution metadata of result.

	// ActionStartTime is the time it took since build start until
	// the action starts. After ActionStartTime, scandeps/retry/fallback
	// may happen and there might be internal waiting time. e.g. remote
	// exec semaphore. ActionStartTime is set within the execution semaphores
	// (localSema, remoteSema, rewrapSema, etc).
	ActionStartTime IntervalMetric `json:"action_start,omitempty"`
	// RunTime is the total duration of the action execution, including
	// overhead such as uploading / downloading files. Semaphore waiting time
	// (namely execution semaphores like localSema, remoteSema, rewrapSema, etc)
	//  is not included.
	RunTime IntervalMetric `json:"run,omitempty"`
	// QueueTime is the time it took until the worker could begin executing
	// the action.
	// TODO: set in reproxy mode too
	QueueTime IntervalMetric `json:"queue,omitempty"`
	// ExecStartTime is set if the action was not cached, containing the time
	// measured when the execution strategy started the process.
	ExecStartTime IntervalMetric `json:"exec_start,omitempty"`
	// InputFetchTime is the time spent on downloading action inputs to the remote
	// worker.
	// It is set only when using remoteexec strategy and no cache.
	// TODO: Measure input fetch time for localexec.
	InputFetchTime IntervalMetric `json:"input_fetch,omitempty"`
	// ExecTime is the time measured from the execution strategy starting
	// the process until the process exited.
	ExecTime IntervalMetric `json:"exec,omitempty"`
	// OutputUploadTime is the time spent on uploading action outputs from
	// the remote worker.
	// It is set only when using remoteexec strategy and no cache.
	OutputUploadTime IntervalMetric `json:"output_upload,omitempty"`
	// ActionEndTime is the time it took since build start until
	// the action completes.
	ActionEndTime IntervalMetric `json:"action_end,omitempty"`

	Inputs  int `json:"inputs,omitempty"`  // how many input files.
	Outputs int `json:"outputs,omitempty"` // how many output files.

	// resource used by local process.
	MaxRSS  int64          `json:"max_rss,omitempty"` // max rss in local cmd.
	Majflt  int64          `json:"majflt,omitempty"`  // major page faults
	Inblock int64          `json:"inblock,omitempty"` // block input operations.
	Oublock int64          `json:"oublock,omitempty"` // block output operations.
	Utime   IntervalMetric `json:"utime,omitempty"`   // user CPU time used for local cmd.
	Stime   IntervalMetric `json:"stime,omitempty"`   // system CPU time used for local cmd.

	skip bool // whether the step was skipped during the build.
}

func (m *StepMetric) init(ctx context.Context, b *Builder, step *Step, stepStart time.Time, prevStepOut string) {
	m.StepID = step.def.String()
	m.Rule = step.def.RuleName()
	m.Action = step.def.ActionName()
	m.Output = b.path.MaybeToWD(ctx, step.def.Outputs(ctx)[0])
	m.GNTarget = step.def.Binding("gn_target")
	m.PrevStepID = step.prevStepID
	m.PrevStepOut = prevStepOut
	m.Ready = IntervalMetric(step.readyTime.Sub(b.start))
	m.Start = IntervalMetric(stepStart.Sub(step.readyTime))
}

func (m *StepMetric) done(ctx context.Context, step *Step, buildStart time.Time) {
	m.WeightedDuration = IntervalMetric(step.getWeightedDuration())
	m.Inputs = len(step.cmd.Inputs)
	m.Outputs = len(step.cmd.Outputs)

	m.CmdHash = base64.StdEncoding.EncodeToString(step.cmd.CmdHash)
	m.Digest = step.cmd.ActionDigest().String()

	result, cached := step.cmd.ActionResult()
	m.Cached = cached
	clog.Infof(ctx, "cached=%t", cached)
	md := result.GetExecutionMetadata()
	if !m.Cached {
		m.QueueTime = IntervalMetric(md.GetWorkerStartTimestamp().AsTime().Sub(md.GetQueuedTimestamp().AsTime()))
		m.ExecStartTime = IntervalMetric(md.GetExecutionStartTimestamp().AsTime().Sub(buildStart))
		m.InputFetchTime = IntervalMetric(md.GetInputFetchCompletedTimestamp().AsTime().Sub(md.GetInputFetchStartTimestamp().AsTime()))
		m.OutputUploadTime = IntervalMetric(md.GetOutputUploadCompletedTimestamp().AsTime().Sub(md.GetOutputUploadStartTimestamp().AsTime()))
	}
	m.ExecTime = IntervalMetric(md.GetExecutionCompletedTimestamp().AsTime().Sub(md.GetExecutionStartTimestamp().AsTime()))
	for _, any := range md.GetAuxiliaryMetadata() {
		ru := &epb.Rusage{}
		err := any.UnmarshalTo(ru)
		if err == nil {
			m.MaxRSS = ru.MaxRss
			m.Majflt = ru.Majflt
			m.Inblock = ru.Inblock
			m.Oublock = ru.Oublock
			m.Utime = IntervalMetric(time.Duration(ru.Utime.Seconds)*time.Second + time.Duration(ru.Utime.Nanos)*time.Nanosecond)
			m.Stime = IntervalMetric(time.Duration(ru.Stime.Seconds)*time.Second + time.Duration(ru.Stime.Nanos)*time.Nanosecond)
		}
	}
}
