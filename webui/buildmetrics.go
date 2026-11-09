// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/metadata"
	"go.chromium.org/build/siso/webui/invocation"
)

// BuildStatus describes whether a build finished, and if so whether it succeeded.
//
// Infer the status from siso_metrics.json. When the build finishes, Siso appends
// a build_id row with an err field if the build failed.
// If the build is killed/crashed/still running, this row doesn't yet exist,
// hence we interpret it as an unknown result.
type BuildStatus string

const (
	buildStatusUnknown BuildStatus = "unknown"
	buildStatusSuccess BuildStatus = "success"
	buildStatusFailure BuildStatus = "failure"
)

// Succeeded returns whether the build finished without an error.
func (s BuildStatus) Succeeded() bool {
	return s == buildStatusSuccess
}

// Failed returns whether the build finished with an error.
func (s BuildStatus) Failed() bool {
	return s == buildStatusFailure
}

// buildMetrics represents data for a single build revision.
// (Exported fields are accessible from Go templates.)
type buildMetrics struct {
	standalone    bool
	mtime         time.Time
	Rev           string // TODO: rename to BuildID?
	Info          *metadata.InvocationInfo
	Status        BuildStatus
	FailedSteps   int
	buildDuration build.IntervalMetric
	lastStepID    string
	ruleCounts    []fieldAggregate
	actionCounts  []fieldAggregate
	// buildMetrics contains build.StepMetric related to overall build e.g. regenerate ninja files.
	buildMetrics []*build.StepMetric
	// stepMetrics contains build.StepMetric related to ninja executions.
	stepMetrics []*build.StepMetric
	// stepByStepID keys step ID to *build.StepMetric for faster lookup.
	stepByStepID map[string]*build.StepMetric
	// stepByOutput keys output to *build.StepMetric for faster lookup.
	stepByOutput map[string]*build.StepMetric
}

// ID returns the build ID of this invocation.
func (b *buildMetrics) ID() string {
	return b.Rev
}

// Started returns when the build started, or a zero value if unknown.
func (b *buildMetrics) Started() invocation.Timestamp {
	if b.Info != nil && !b.Info.StartTime.IsZero() {
		return invocation.Timestamp{Time: b.Info.StartTime, Inferred: false}
	}
	// For local outdir builds where InvocationInfo is missing (e.g. older builds
	// before siso_metadata.json was introduced), approximate the start time by
	// subtracting the build duration from the metrics file modification time (build end).
	if !b.standalone && !b.mtime.IsZero() {
		return invocation.Timestamp{
			Time:     b.mtime.Add(-time.Duration(b.buildDuration)),
			Inferred: true,
		}
	}
	// Standalone/uploaded metrics without metadata (or builds without mtime)
	// return a zero timestamp because their local mtime only reflects when the
	// file was saved or imported.
	return invocation.Timestamp{}
}

// BuildDuration returns the build duration of this invocation.
func (b *buildMetrics) BuildDuration() build.IntervalMetric {
	return b.buildDuration
}

// StepMetrics returns the step metrics structs for this invocation.
func (b *buildMetrics) StepMetrics() []*build.StepMetric {
	return b.stepMetrics
}

// CriticalPath returns the build steps on the critical path in execution order.
func (b *buildMetrics) CriticalPath() []*build.StepMetric {
	var path []*build.StepMetric
	// We assume the last step is on the critical path.
	// Build the critical path backwards then reverse it.
	critStepID := b.lastStepID
	for critStepID != "" {
		if step, ok := b.stepByStepID[critStepID]; ok {
			path = append(path, step)
			critStepID = step.PrevStepID
		} else {
			// TODO(b/349287453): add some sort of error to indicate prev step was not found
			break
		}
	}
	slices.Reverse(path)
	return path
}

func loadBuildMetrics(metricsPath string) (*buildMetrics, error) {
	f, err := os.Open(metricsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metrics: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat metrics: %w", err)
	}

	metricsData := &buildMetrics{
		mtime:        stat.ModTime(),
		buildMetrics: []*build.StepMetric{},
		stepMetrics:  []*build.StepMetric{},
		stepByStepID: make(map[string]*build.StepMetric),
		stepByOutput: make(map[string]*build.StepMetric),
		ruleCounts:   []fieldAggregate{},
		actionCounts: []fieldAggregate{},
	}

	d := json.NewDecoder(f)
	buildFinished := false
	for {
		var m build.StepMetric
		err := d.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			buildFinished = false
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse error in %s:%d: %w", metricsPath, d.InputOffset(), err)
		}
		if m.BuildID != "" {
			metricsData.buildMetrics = append(metricsData.buildMetrics, &m)
			// The last build metric found has the actual build duration.
			metricsData.buildDuration = m.Duration
			buildFinished = true
		} else if m.StepID != "" {
			metricsData.stepMetrics = append(metricsData.stepMetrics, &m)
			metricsData.stepByStepID[m.StepID] = &m
			metricsData.stepByOutput[m.Output()] = &m
			metricsData.lastStepID = m.StepID
			if m.Err {
				metricsData.FailedSteps++
			}
			buildFinished = false
		} else {
			return nil, fmt.Errorf("unexpected metric found %v", m)
		}
	}

	if len(metricsData.buildMetrics) == 0 || metricsData.buildMetrics[0].BuildID == "" {
		return nil, fmt.Errorf("need at least one build_id in %s", metricsPath)
	}
	metricsData.Rev = metricsData.buildMetrics[0].BuildID

	switch {
	case !buildFinished:
		metricsData.Status = buildStatusUnknown
	case metricsData.buildMetrics[len(metricsData.buildMetrics)-1].Err:
		metricsData.Status = buildStatusFailure
	default:
		metricsData.Status = buildStatusSuccess
	}

	actionCounts := make(map[string]int)
	for _, metric := range metricsData.stepMetrics {
		if metric.Action != "" {
			actionCounts[metric.Action]++
		}
	}
	for action := range actionCounts {
		metricsData.actionCounts = append(metricsData.actionCounts, fieldAggregate{
			Key:   action,
			Count: actionCounts[action],
		})
	}
	slices.SortFunc(metricsData.actionCounts, func(a, b fieldAggregate) int {
		return cmp.Compare(b.Count, a.Count)
	})

	ruleCounts := make(map[string]int)
	for _, metric := range metricsData.stepMetrics {
		if metric.Rule != "" {
			ruleCounts[metric.Rule]++
		}
	}
	for rule := range ruleCounts {
		metricsData.ruleCounts = append(metricsData.ruleCounts, fieldAggregate{
			Key:   rule,
			Count: ruleCounts[rule],
		})
	}
	slices.SortFunc(metricsData.ruleCounts, func(a, b fieldAggregate) int {
		return cmp.Compare(b.Count, a.Count)
	})

	// Attempt to load corresponding invocation metadata if available.
	dir := filepath.Dir(metricsPath)
	if matches, err := filepath.Glob(filepath.Join(dir, "siso_metadata*.json")); err == nil {
		for _, match := range matches {
			data, err := os.ReadFile(match)
			if err != nil {
				continue
			}
			var info metadata.InvocationInfo
			if err := json.Unmarshal(data, &info); err != nil {
				continue
			}
			if info.BuildID == metricsData.Rev {
				metricsData.Info = &info
				break
			}
		}
	}

	return metricsData, nil
}
