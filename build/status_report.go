// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

// StatusReporter is an interface to report build status.
type StatusReporter interface {
	// PlanHasTotalSteps is called when total steps is updated.
	PlanHasTotalSteps(total int)

	// BuildActionStarted is called when build action started.
	BuildActionStarted(*Step)

	// BuildActionFinished is called when build action finished.
	BuildActionFinished(*Step)

	// BuildActionCanceled is called when build action canceled.
	BuildActionCanceled(*Step)

	// BuildStarted is called when build started.
	BuildStarted()

	// BuildFinished is called when build finished.
	BuildFinished()
}

type noopStatusReporter struct{}

func (noopStatusReporter) PlanHasTotalSteps(total int) {}

func (noopStatusReporter) BuildActionStarted(step *Step)  {}
func (noopStatusReporter) BuildActionFinished(step *Step) {}
func (noopStatusReporter) BuildActionCanceled(step *Step) {}

func (noopStatusReporter) BuildStarted()  {}
func (noopStatusReporter) BuildFinished() {}
