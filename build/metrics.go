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

	log "github.com/golang/glog"

	epb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/o11y/clog"
	rbepb "go.chromium.org/build/siso/reapi/proto"
)

// IntervalMetric is a time duration, but serialized as nanoseconds in JSON.
type IntervalMetric time.Duration

// MarshalJSON marshals the IntervalMetric as int64 of nanoseconds.
func (i IntervalMetric) MarshalJSON() ([]byte, error) {
	d := time.Duration(i)
	return []byte(strconv.FormatInt(d.Nanoseconds(), 10)), nil
}

// UnmarshalJSON unmarshals int64 of nanoseconds as an IntervalMetric.
func (i *IntervalMetric) UnmarshalJSON(b []byte) error {
	var nanosecs int64
	err := json.Unmarshal(b, &nanosecs)
	if err != nil {
		return err
	}
	*i = IntervalMetric(time.Duration(nanosecs))
	return nil
}

// StepMetric contains metrics about a build Step.
type StepMetric struct {
	BuildID string `json:"build_id,omitempty"` // the unique id of the current build (trace)
	StepID  string `json:"step_id,omitempty"`  // the unique id of this step (top span)

	Rule     string   `json:"rule,omitempty"`      // the rule name of the step
	Action   string   `json:"action,omitempty"`    // the action name of the step
	Outputs  []string `json:"outputs,omitempty"`   // a list of the output files of the step.
	GNTarget string   `json:"gn_target,omitempty"` // inferred gn target

	// The ID of the previous step.
	// The "previous" step is defined as the last step that updated
	// the output that is used as part of this step's inputs.
	PrevStepID string `json:"prev,omitempty"`

	// Ready, Start and Duration are measured by Siso's scheduler,
	// independently of the measurements provided by the execution
	// strategies (see RunTime, QueueTime, ExecTime below).

	// Ready is the time it took since build start until the action became
	// ready for execution (= all inputs are available).
	Ready IntervalMetric `json:"ready_nanos,omitempty"`
	// Start is the time it took until Siso's scheduler was ready to work
	// on the step (concurrency limited by stepSema) and pass it to an
	// execution strategy.
	Start IntervalMetric `json:"start_nanos,omitempty"`
	// Duration is the time it took for the action to do its job, measured
	// from start of work until it is completed.
	// It includes siso-overhead (preproc etc) and command executon
	// (RunTime).
	// for full build metric, it's duration to process all scheduled steps.
	Duration IntervalMetric `json:"duration_nanos,omitempty"`

	// WeightedDuration is an estimate of the "true duration" of the action
	// that tries to accommodate for the impact of other actions running in
	// parallel. It is calculated by summing up small slices of time (~100ms)
	// while the action is running, where each slice's duration is divided by
	// the number of concurrently running actions at that point in time.
	WeightedDuration IntervalMetric `json:"weighted_duration_nanos,omitempty"`

	// The hash of the command-line of the build step.
	CmdHash string `json:"cmdhash,omitempty"`
	// The hash of the action proto of this build step.
	Digest string `json:"digest,omitempty"`

	ScandepsErr   bool `json:"scandeps_err,omitempty"`   // whether the action failed in scandeps.
	ClangScandeps bool `json:"clang_scandeps,omitempty"` // whether the action used the clang for scandeps.

	NoExec        bool   `json:"no_exec,omitempty"`         // whether the action didn't run any command (i.e. just use handler).
	IsRemote      bool   `json:"is_remote,omitempty"`       // whether the action uses remote result.
	IsLocal       bool   `json:"is_local,omitempty"`        // whether the action uses local result.
	Sandbox       bool   `json:"sandbox,omitempty"`         // whether the action uses sandbox.
	FastLocal     bool   `json:"fast_local,omitempty"`      // whether the action chooses local for fast build.
	StartLocal    bool   `json:"start_local,omitempty"`     // whether the action chooses local for start in incremental build.
	CacheWrite    bool   `json:"cache_write,omitempty"`     // whether the action used cache write feature from local results.
	CacheWriteErr bool   `json:"cache_write_err,omitempty"` // whether the action failed while using cache write feature.
	Cached        bool   `json:"cached,omitempty"`          // whether the action was a cache hit.
	Fallback      bool   `json:"fallback,omitempty"`        // whether the action failed remotely and was retried locally.
	Racing        bool   `json:"racing,omitempty"`          // whether the action used racing (local+remote in parallel).
	RacingWinner  string `json:"racing_winner,omitempty"`   // "local" or "remote" if racing was used.
	Err           bool   `json:"err,omitempty"`             // whether the action failed.
	RemoteRetry   int    `json:"remote_retry,omitempty"`    // count of remote retry
	WorkerPool    string `json:"worker_pool,omitempty"`     // worker pool that executes the action.

	// DepsScanTime is the time it took in calculating deps for cmd inputs.
	// Semaphore waiting time is included, which does not count towards ActionStartTime.
	// Use ScandepsTime instead if semaphore waiting time should be excluded,
	// or a measurement that starts within ActionStartTime is required.
	DepsScanTime IntervalMetric `json:"depsscan_nanos,omitempty"`

	// RunTime, QueueTime and ExecTime are measured by the execution
	// strategies in execution metadata of result.

	// ActionStartTime is the time it took since build start until
	// the action starts. After ActionStartTime, scandeps/retry/fallback
	// may happen and there might be internal waiting time. e.g. remote
	// exec semaphore. ActionStartTime is set within the execution semaphores
	// (localSema, remoteSema, rewrapSema, etc).
	ActionStartTime IntervalMetric `json:"action_start_nanos,omitempty"`

	// ScandepsStartTime is the time it took since build start until
	// scandeps starts.
	ScandepsStartTime IntervalMetric `json:"scandeps_start_nanos,omitempty"`
	// ScandepsTime is the duration measured from the execution strategy
	// starting the scandeps process until the scandeps process exited.
	// Semaphore waiting time is excluded.
	// Use DepsScanTime instead if semaphore waiting time is desired.
	ScandepsTime IntervalMetric `json:"scandeps_nanos,omitempty"`

	// CacheStartTime is the time it took since build start until
	// querying the remote cache starts.
	CacheStartTime IntervalMetric `json:"cache_start_nanos,omitempty"`
	// CacheTime is the duration measured from the execution strategy
	// starting the remote cache query until it finishes.  For cache hits,
	// this includes the time to process the hit.
	CacheTime IntervalMetric `json:"cache_nanos,omitempty"`

	// MaterializeInputsTime is the time it took to materialize inputs to disk
	// that were required by the step.
	MaterializeInputsTime IntervalMetric `json:"materialize_inputs_nanos,omitempty"`
	// MaterializeOutputsTime is the time it took to materialize outputs to disk
	// by the step.
	// These could be remote files from CAS, or local in-memory files from a
	// step handler, etc.
	MaterializeOutputsTime IntervalMetric `json:"materialize_outputs_nanos,omitempty"`

	// RunTime is the total duration of the action execution, including
	// overhead such as uploading / downloading files. Semaphore waiting time
	// (namely execution semaphores like localSema, remoteSema, rewrapSema, etc)
	//  is not included.
	RunTime IntervalMetric `json:"run_nanos,omitempty"`
	// QueueTime is the time it took until the worker could begin executing
	// the action.
	QueueTime IntervalMetric `json:"queue_nanos,omitempty"`
	// ExecStartTime is set if the action was not cached, containing the time
	// measured when the execution strategy started the process.
	ExecStartTime IntervalMetric `json:"exec_start_nanos,omitempty"`
	// InputFetchTime is the time spent on downloading action inputs to the remote
	// worker.
	// It is set only when using remoteexec strategy and no cache.
	// TODO: Measure input fetch time for localexec.
	InputFetchTime IntervalMetric `json:"input_fetch_nanos,omitempty"`
	// ExecTime is the time measured from the execution strategy starting
	// the process until the process exited.
	ExecTime IntervalMetric `json:"exec_nanos,omitempty"`

	// WorkerTime is the time measured from when the worker started the process
	// until the worker completed the process (including input fetch and output upload time and
	// other miscellaneous overheads that aren't measured individually).
	WorkerTime IntervalMetric `json:"worker_nanos,omitempty"`
	// OutputUploadTime is the time spent on uploading action outputs from
	// the remote worker.
	// It is set only when using remoteexec strategy and no cache.
	OutputUploadTime IntervalMetric `json:"output_upload_nanos,omitempty"`
	// ActionEndTime is the time it took since build start until
	// the action completes.
	ActionEndTime IntervalMetric `json:"action_end_nanos,omitempty"`

	Inputs int `json:"inputs,omitempty"` // how many input files.

	// resource used by local process.
	MaxRSS  int64          `json:"max_rss,omitempty"`     // max rss in local cmd.
	Majflt  int64          `json:"majflt,omitempty"`      // major page faults
	Inblock int64          `json:"inblock,omitempty"`     // block input operations.
	Oublock int64          `json:"oublock,omitempty"`     // block output operations.
	Utime   IntervalMetric `json:"utime_nanos,omitempty"` // user CPU time used for local cmd.
	Stime   IntervalMetric `json:"stime_nanos,omitempty"` // system CPU time used for local cmd.

	skip bool // whether the step was skipped during the build.
}

// Output returns the first output from Outputs.
func (m StepMetric) Output() string {
	if len(m.Outputs) == 0 {
		return ""
	}
	return m.Outputs[0]
}

// copyExecResult copies execution metrics from a local racing clone back
// to the original step's metrics. This includes both the fields set
// during command execution and the fields set by done().
func (m *StepMetric) copyExecResult(src *StepMetric) {
	// Fields set by done().
	m.Inputs = src.Inputs
	m.Outputs = src.Outputs
	m.CmdHash = src.CmdHash
	m.Digest = src.Digest

	// Fields set during command execution.
	m.RunTime = src.RunTime
	m.ExecTime = src.ExecTime
	m.ActionStartTime = src.ActionStartTime
	m.Cached = src.Cached
	m.CacheWrite = src.CacheWrite
	m.CacheWriteErr = src.CacheWriteErr
	m.MaxRSS = src.MaxRSS
	m.Majflt = src.Majflt
	m.Inblock = src.Inblock
	m.Oublock = src.Oublock
	m.Utime = src.Utime
	m.Stime = src.Stime
}

func (m *StepMetric) init(ctx context.Context, b *Builder, step *Step, stepStart time.Time) {
	m.StepID = step.def.String()
	m.Rule = step.def.RuleName()
	m.Action = step.def.ActionName()
	for _, o := range step.def.Outputs(ctx) {
		m.Outputs = append(m.Outputs, b.path.MaybeToRelative(ctx, o))
	}
	m.GNTarget = step.def.Binding("gn_target")
	m.PrevStepID = step.prevStepID
	m.Ready = IntervalMetric(step.readyTime.Sub(b.start))
	m.Start = IntervalMetric(stepStart.Sub(step.readyTime))
}

func (m *StepMetric) done(ctx context.Context, step *Step, buildStart time.Time) {
	m.WeightedDuration = IntervalMetric(step.getWeightedDuration())
	m.Inputs = len(step.cmd.Inputs)

	m.CmdHash = base64.StdEncoding.EncodeToString(step.cmd.CmdHash)
	m.Digest = step.cmd.ActionDigest().String()

	result, cached := step.cmd.ActionResult()
	m.Cached = cached
	if log.V(1) {
		clog.Infof(ctx, "cached=%t", cached)
	}
	md := result.GetExecutionMetadata()
	if !m.Cached {
		var queueEnd time.Time
		if md.GetWorkerStartTimestamp() == nil {
			queueEnd = md.GetExecutionStartTimestamp().AsTime()
		} else {
			queueEnd = md.GetWorkerStartTimestamp().AsTime()
		}
		m.QueueTime = IntervalMetric(queueEnd.Sub(md.GetQueuedTimestamp().AsTime()))
		m.ExecStartTime = IntervalMetric(md.GetExecutionStartTimestamp().AsTime().Sub(buildStart))
		m.InputFetchTime = IntervalMetric(md.GetInputFetchCompletedTimestamp().AsTime().Sub(md.GetInputFetchStartTimestamp().AsTime()))
		m.OutputUploadTime = IntervalMetric(md.GetOutputUploadCompletedTimestamp().AsTime().Sub(md.GetOutputUploadStartTimestamp().AsTime()))
		m.ExecTime = IntervalMetric(md.GetExecutionCompletedTimestamp().AsTime().Sub(md.GetExecutionStartTimestamp().AsTime()))

		if md.GetWorkerStartTimestamp() != nil && md.GetWorkerCompletedTimestamp() != nil {
			m.WorkerTime = IntervalMetric(md.GetWorkerCompletedTimestamp().AsTime().Sub(md.GetWorkerStartTimestamp().AsTime()))
		}
	}
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
			continue
		}
		// Get worker pool for executed action.
		if !m.Cached {
			aux := &rbepb.AuxiliaryMetadata{}
			err = any.UnmarshalTo(aux)
			if err == nil {
				if pool := aux.GetPool(); pool != "" {
					m.WorkerPool = pool
				}
				continue
			}
		}
	}
}
