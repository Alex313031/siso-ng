// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/ui"
)

// quietUI implements ui.UI and build.StatusReporter,
// and just shows command outputs.
type quietUI struct {
	heartbeatPeriod time.Duration
}

var _ build.StatusReporter = quietUI{}

func (quietUI) PlanHasTotalSteps(int)                     {}
func (quietUI) BuildActionStarted(*build.Step, time.Time) {}

func (quietUI) BuildActionFinished(step *build.Step) {
	os.Stderr.Write(step.Stderr())
	os.Stdout.Write(step.Stdout())
}

func (quietUI) BuildActionCanceled(*build.Step) {}

func (quietUI) BuildStarted()  {}
func (quietUI) BuildFinished() {}

var _ ui.UI = quietUI{}

func (quietUI) PrintLines(...string) {}
func (ui quietUI) NewSpinner() ui.Spinner {
	return &quietSpinner{
		heartbeatPeriod: ui.heartbeatPeriod,
	}
}
func (quietUI) Printf(string, ...any)   {}
func (quietUI) Infof(string, ...any)    {}
func (quietUI) Warningf(string, ...any) {}
func (quietUI) Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

type quietSpinner struct {
	cancel          context.CancelFunc
	heartbeatPeriod time.Duration
}

func (q *quietSpinner) Start(string, ...any) {
	if q.heartbeatPeriod == 0 {
		return
	}

	// Run a goroutine that will print the heartbeat on stdout.
	// Use a separate context that gets canceled upon function exit.
	heartbeatContext, cancel := context.WithCancel(context.Background())
	q.cancel = cancel

	go func(ctx context.Context) {
		ticker := time.Tick(q.heartbeatPeriod)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker:
				fmt.Fprintf(os.Stdout, ".")
			}
		}
	}(heartbeatContext)
}

func (q *quietSpinner) Stop(error) {
	if q.cancel != nil {
		q.cancel()
	}
}

func (q *quietSpinner) Done(string, ...any) {
	if q.cancel != nil {
		q.cancel()
	}
}
