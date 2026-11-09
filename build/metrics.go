// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	log "github.com/golang/glog"

	epb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	rbepb "go.chromium.org/build/siso/reapi/proto"
)

// Span names used for metric purposes.
const (
	// Total span for step preproc.
	spanPreproc = "preproc"
	// Child span of [spanPreproc] for depsCmd.
	// This is a temporary name and is subject to change, do not rely on this.
	spanDepsCmd = "deps-cmd"
	// Child span of [spanDepsCmd] measuring after acquiring the scandeps semaphore.
	spanScandepsRun = "scandeps-run"

	// Total span for remote cache.
	spanExecRemoteCache = "exec-remote-cache"
	// Child span of [spanExecRemoteCache] measuring starting the remote cache query until it
	// finishes, i.e. excludes cacheSema wait time.
	spanExecRemoteCacheCheck = "exec-remote-cache-check"

	// Span for time it took to materialize inputs to disk
	// that were required by the step.
	spanMaterializeInputs = "materialize-inputs"
	// Span for time it took to materialize outputs to disk
	// that were required by the step.
	spanMaterializeOutputs = "materialize-outputs"

	// Run spans.
	// Exactly one set of these is canonical for purposes of reporting a step's "run time"
	// for comparison purposes including ninja reports.
	// Unlike ninja, execution strategies may be repeated, run in parallel, etc. so
	// it is not surprising to see more than one of these spans logged for a step.

	// Span for local execution of the step, measured from the start of command until
	// post-processing (deps update, output check, cache write, output flush) completes.
	// Semaphore waiting time is not included.
	spanExecLocalRun = "exec-local-run"

	// Span for remote cache hit of the step, measured from the start of cache query
	// until post-processing (deps update, output flush) completes.
	// Semaphore waiting time is not included.
	spanExecRemoteCacheRun = "exec-remote-cache-run"

	// Span for a step completed by a handler without running a command, measured for
	// the handler call.
	spanHandleStepRun = "handle-step-run"

	// Span for a single remote execution attempt. Multiple of these may be logged.
	// This does not include post-processing.
	spanExecRemoteExecAttempt = "exec-remote-exec-attempt"
	// Span for remote execution post-processing (output download, deps update, output flush).
	spanExecRemoteExecPostProc = "exec-remote-exec-postproc"
)

// shouldLogSpan returns whether this is a span that should be logged for metric purposes.
func shouldLogSpan(name string) bool {
	kind, _, _ := strings.Cut(name, "/")
	if strings.HasPrefix(kind, "step:") {
		return true
	}
	switch kind {
	case spanScandepsRun,
		spanExecLocalRun,
		spanExecRemoteCacheCheck,
		spanExecRemoteCacheRun,
		spanExecRemoteExecAttempt,
		spanHandleStepRun,
		spanMaterializeInputs,
		spanMaterializeOutputs:
		return true
	}
	return false
}

// MetricSpan is span timing logged for metric purposes.
type MetricSpan struct {
	Name          string `json:"name"`
	StartNanos    int64  `json:"start_ns"`
	DurationNanos int64  `json:"duration_ns"`
}

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

	// The ID of the step that updated one of this step's inputs second to
	// last, i.e. the step that would have released this step had the
	// "previous" step finished earlier. Empty when at most one distinct
	// step released this step's inputs.
	Prev2StepID string `json:"prev2,omitempty"`

	// Ready, Start and Duration are measured by Siso's scheduler,
	// independently of the measurements provided by the execution
	// strategies (see RunTime, QueueTime, ExecTime below).

	// Ready is the time it took since build start until the action became
	// ready for execution (= all inputs are available).
	Ready IntervalMetric `json:"ready_nanos,omitzero"`
	// Start is the time it took until Siso's scheduler was ready to work
	// on the step (concurrency limited by stepSema) and pass it to an
	// execution strategy.
	Start IntervalMetric `json:"start_nanos,omitzero"`
	// Duration is the time it took for the action to do its job, measured
	// from start of work until it is completed.
	// It includes siso-overhead (preproc etc) and command executon
	// (RunTime).
	// for full build metric, it's duration to process all scheduled steps.
	Duration IntervalMetric `json:"duration_nanos,omitzero"`

	// WeightedDuration is an estimate of the "true duration" of the action
	// that tries to accommodate for the impact of other actions running in
	// parallel. It is calculated by summing up small slices of time (~100ms)
	// while the action is running, where each slice's duration is divided by
	// the number of concurrently running actions at that point in time.
	WeightedDuration IntervalMetric `json:"weighted_duration_nanos,omitzero"`

	// The hash of the command-line of the build step.
	CmdHash string `json:"cmdhash,omitempty"`
	// The hash of the action proto of this build step.
	Digest string `json:"digest,omitempty"`

	ScandepsErr   bool `json:"scandeps_err,omitzero"`   // whether the action failed in scandeps.
	ClangScandeps bool `json:"clang_scandeps,omitzero"` // whether the action used the clang for scandeps.

	NoExec        bool   `json:"no_exec,omitzero"`         // whether the action didn't run any command (i.e. just use handler).
	IsRemote      bool   `json:"is_remote,omitzero"`       // whether the action uses remote result.
	IsLocal       bool   `json:"is_local,omitzero"`        // whether the action uses local result.
	Sandbox       bool   `json:"sandbox,omitzero"`         // whether the action uses sandbox.
	FastLocal     bool   `json:"fast_local,omitzero"`      // whether the action chooses local for fast build.
	StartLocal    bool   `json:"start_local,omitzero"`     // whether the action chooses local for start in incremental build.
	CacheWrite    bool   `json:"cache_write,omitzero"`     // whether the action used cache write feature from local results.
	CacheWriteErr bool   `json:"cache_write_err,omitzero"` // whether the action failed while using cache write feature.
	Cached        bool   `json:"cached,omitzero"`          // whether the action was a cache hit.
	Fallback      bool   `json:"fallback,omitzero"`        // whether the action failed remotely and was retried locally.
	Racing        bool   `json:"racing,omitzero"`          // whether the action used racing (local+remote in parallel).
	RacingWinner  string `json:"racing_winner,omitempty"`  // "local" or "remote" if racing was used.
	Err           bool   `json:"err,omitzero"`             // whether the action failed.
	RemoteRetry   int    `json:"remote_retry,omitzero"`    // count of remote retry
	WorkerPool    string `json:"worker_pool,omitempty"`    // worker pool that executes the action.

	Spans []MetricSpan `json:"spans,omitempty"`

	// DepsScanTime is the time it took in calculating deps for cmd inputs.
	// Semaphore waiting time is included, which does not count towards ActionStartTime.
	// Use ScandepsTime instead if semaphore waiting time should be excluded,
	// or a measurement that starts within ActionStartTime is required.
	// TODO: not accurate name as it covers all of depsCmd, change name?
	DepsScanTime IntervalMetric `json:"depsscan_nanos,omitzero"`

	// RunTime, QueueTime and ExecTime are measured by the execution
	// strategies in execution metadata of result.

	// ActionStartTime is the time it took since build start until
	// the action starts. After ActionStartTime, scandeps/retry/fallback
	// may happen and there might be internal waiting time. e.g. remote
	// exec semaphore. ActionStartTime is set within the execution semaphores
	// (localSema, remoteSema, rewrapSema, etc).
	ActionStartTime IntervalMetric `json:"action_start_nanos,omitzero"`

	// ScandepsStartTime is the time it took since build start until
	// scandeps starts.
	ScandepsStartTime IntervalMetric `json:"scandeps_start_nanos,omitzero"`
	// ScandepsTime is the duration measured from the execution strategy
	// starting the scandeps process until the scandeps process exited.
	// Semaphore waiting time is excluded.
	// Use DepsScanTime instead if semaphore waiting time is desired.
	ScandepsTime IntervalMetric `json:"scandeps_nanos,omitzero"`

	// CacheStartTime is the time it took since build start until
	// querying the remote cache starts.
	CacheStartTime IntervalMetric `json:"cache_start_nanos,omitzero"`
	// CacheTime is the duration measured from the execution strategy
	// starting the remote cache query until it finishes.  For cache hits,
	// this includes the time to process the hit.
	CacheTime IntervalMetric `json:"cache_nanos,omitzero"`

	// MaterializeInputsTime is the time it took to materialize inputs to disk
	// that were required by the step.
	//
	// Deprecated: Being migrated to spans, as this provides granularity
	// as to when the materialize is being called.
	MaterializeInputsTime IntervalMetric `json:"materialize_inputs_nanos,omitzero"`
	// MaterializeOutputsTime is the time it took to materialize outputs to disk
	// by the step.
	// These could be remote files from CAS, or local in-memory files from a
	// step handler, etc.
	//
	// Deprecated: Being migrated to spans, as this provides granularity
	// as to when the materialize is being called.
	MaterializeOutputsTime IntervalMetric `json:"materialize_outputs_nanos,omitzero"`

	// Cache lookup key for two phase caching.
	TwoPhaseCachingKey string `json:"two_phase_caching_key,omitempty"`
	// Number of actions checked for two phase caching.
	TwoPhaseCachingActions int  `json:"two_phase_caching_actions,omitzero"`
	TwoPhaseCacheHit       bool `json:"two_phase_cache_hit,omitzero"`

	// RunTime is the total duration of the action execution, including
	// overhead such as uploading / downloading files. Semaphore waiting time
	// (namely execution semaphores like localSema, remoteSema, rewrapSema, etc)
	//  is not included.
	RunTime IntervalMetric `json:"run_nanos,omitzero"`
	// QueueTime is the time it took until the worker could begin executing
	// the action.
	QueueTime IntervalMetric `json:"queue_nanos,omitzero"`
	// ExecStartTime is set if the action was not cached, containing the time
	// measured when the execution strategy started the process.
	ExecStartTime IntervalMetric `json:"exec_start_nanos,omitzero"`
	// InputFetchTime is the time spent on downloading action inputs to the remote
	// worker.
	// It is set only when using remoteexec strategy and no cache.
	// TODO: Measure input fetch time for localexec.
	InputFetchTime IntervalMetric `json:"input_fetch_nanos,omitzero"`
	// ExecTime is the time measured from the execution strategy starting
	// the process until the process exited.
	ExecTime IntervalMetric `json:"exec_nanos,omitzero"`

	// WorkerTime is the time measured from when the worker started the process
	// until the worker completed the process (including input fetch and output upload time and
	// other miscellaneous overheads that aren't measured individually).
	WorkerTime IntervalMetric `json:"worker_nanos,omitzero"`
	// OutputUploadTime is the time spent on uploading action outputs from
	// the remote worker.
	// It is set only when using remoteexec strategy and no cache.
	OutputUploadTime IntervalMetric `json:"output_upload_nanos,omitzero"`
	// ActionEndTime is the time it took since build start until
	// the action completes.
	ActionEndTime IntervalMetric `json:"action_end_nanos,omitzero"`

	Inputs int `json:"inputs,omitzero"` // how many input files.

	// resource used by local process.
	MaxRSS  int64          `json:"max_rss,omitzero"`     // max rss in local cmd.
	Majflt  int64          `json:"majflt,omitzero"`      // major page faults
	Inblock int64          `json:"inblock,omitzero"`     // block input operations.
	Oublock int64          `json:"oublock,omitzero"`     // block output operations.
	Utime   IntervalMetric `json:"utime_nanos,omitzero"` // user CPU time used for local cmd.
	Stime   IntervalMetric `json:"stime_nanos,omitzero"` // system CPU time used for local cmd.

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
	m.GNTarget = step.def.Binding("gn_target")
	m.PrevStepID = step.prevStepIDs[0]
	m.Prev2StepID = step.prevStepIDs[1]
	m.Ready = IntervalMetric(step.readyTime.Sub(b.start))
	m.Start = IntervalMetric(stepStart.Sub(step.readyTime))
}

func (m *StepMetric) done(ctx context.Context, b *Builder, step *Step, buildStart time.Time) {
	m.WeightedDuration = IntervalMetric(step.getWeightedDuration())
	m.Inputs = len(step.cmd.Inputs)

	seen := make(map[string]bool)
	var outputs []string
	for _, o := range step.cmd.Outputs {
		s := b.path.MaybeToRelative(ctx, string(o))
		if !seen[s] {
			seen[s] = true
			outputs = append(outputs, s)
		}
	}
	for _, o := range step.cmd.OutputDirs {
		s := b.path.MaybeToRelative(ctx, string(o))
		if !strings.HasSuffix(s, "/") {
			s += "/"
		}
		if !seen[s] {
			seen[s] = true
			outputs = append(outputs, s)
		}
	}
	if len(outputs) > 0 {
		m.Outputs = outputs
	}

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

// updateFromTrace updates metrics from the provided trace spans.
func (m *StepMetric) updateFromTrace(spans []trace.SpanData, buildStart time.Time) {
	var metricSpans []MetricSpan
	// There can be multiple run time spans per step, but only one canonical RunTime.
	// Collect all of them upfront and decide the winner after the loop.
	var localRun, remoteCacheRun, handleRun IntervalMetric
	var remoteAttempt, remotePostProc time.Duration
	var remoteAttemptStart time.Time

	for _, s := range spans {
		if shouldLogSpan(s.Name) {
			metricSpans = append(metricSpans, MetricSpan{
				Name:          s.Name,
				StartNanos:    s.Start.Sub(buildStart).Nanoseconds(),
				DurationNanos: s.Duration().Nanoseconds(),
			})
		}
		switch s.Name {
		case spanDepsCmd:
			m.DepsScanTime = IntervalMetric(s.Duration())
		case spanScandepsRun:
			m.ScandepsTime = IntervalMetric(s.Duration())
			m.ScandepsStartTime = IntervalMetric(s.Start.Sub(buildStart))
		case spanExecLocalRun:
			localRun = IntervalMetric(s.Duration())
		case spanExecRemoteCacheCheck:
			m.CacheTime = IntervalMetric(s.Duration())
			m.CacheStartTime = IntervalMetric(s.Start.Sub(buildStart))
		case spanExecRemoteCacheRun:
			remoteCacheRun = IntervalMetric(s.Duration())
		case spanExecRemoteExecAttempt:
			if s.Start.After(remoteAttemptStart) {
				remoteAttempt = s.Duration()
				remoteAttemptStart = s.Start
			}
		case spanExecRemoteExecPostProc:
			remotePostProc = s.Duration()
		case spanHandleStepRun:
			handleRun = IntervalMetric(s.Duration())
		case spanMaterializeInputs:
			m.MaterializeInputsTime = IntervalMetric(s.Duration())
		case spanMaterializeOutputs:
			m.MaterializeOutputsTime = IntervalMetric(s.Duration())
		}
	}
	m.Spans = metricSpans

	switch {
	case m.NoExec:
		// NoExec means no command was run (i.e. handler used).
		m.RunTime = handleRun
	case m.IsLocal:
		// Local fallback after failed remoteexec means both IsRemote and IsLocal
		// will be set, so check IsLocal first.
		m.RunTime = localRun
	case m.IsRemote:
		// Remote execution.
		m.RunTime = IntervalMetric(remoteAttempt + remotePostProc)
	case m.Cached:
		// Remote cache hit.
		m.RunTime = remoteCacheRun
	}
}
