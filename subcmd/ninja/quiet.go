// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/ui"
)

// quietUI implements ui.UI and build.StatusReporter,
// and just shows command outputs.
type quietUI struct {
	// Stdout and Stderr specify optional destination writers (used for testing).
	// If nil, os.Stdout and os.Stderr are used respectively.
	Stdout, Stderr  io.Writer
	heartbeatPeriod time.Duration
}

func (q quietUI) stdoutWriter() io.Writer {
	if q.Stdout != nil {
		return q.Stdout
	}
	return os.Stdout
}

func (q quietUI) stderrWriter() io.Writer {
	if q.Stderr != nil {
		return q.Stderr
	}
	return os.Stderr
}

var _ build.StatusReporter = quietUI{}

func (quietUI) PlanHasTotalSteps(int)                     {}
func (quietUI) BuildActionStarted(*build.Step, time.Time) {}

func (q quietUI) BuildActionFinished(step *build.Step) {
	if step.ExitCode() != 0 {
		q.stderrWriter().Write([]byte(step.OutputResult()))
		return
	}
	q.stderrWriter().Write(step.Stderr())
	q.stdoutWriter().Write(step.Stdout())
}

func (quietUI) BuildActionCanceled(*build.Step) {}

func (quietUI) BuildStarted()  {}
func (quietUI) BuildFinished() {}

var _ ui.UI = quietUI{}

func (quietUI) PrintLines(...string)          {}
func (quietUI) IsOneTerminalLine(string) bool { return true }
func (q quietUI) NewSpinner() ui.Spinner {
	return &quietSpinner{
		w:               q.stdoutWriter(),
		heartbeatPeriod: q.heartbeatPeriod,
	}
}
func (quietUI) Printf(string, ...any)   {}
func (quietUI) Infof(string, ...any)    {}
func (quietUI) Warningf(string, ...any) {}
func (q quietUI) Errorf(format string, args ...any) {
	fmt.Fprintf(q.stderrWriter(), format, args...)
}

func (q quietUI) printPsHint(outDir string) {
	cmd := "siso ps"
	if outDir != "." && outDir != "" {
		cmd = fmt.Sprintf("siso ps -C %s", outDir)
	}
	fmt.Fprintf(q.stderrWriter(), "Run `%s` in another terminal to see build status.\n", cmd)
}

type quietSpinner struct {
	w               io.Writer
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	heartbeatPeriod time.Duration
}

func (q *quietSpinner) writer() io.Writer {
	if q.w != nil {
		return q.w
	}
	return os.Stdout
}

func (q *quietSpinner) Start(string, ...any) {
	if q.heartbeatPeriod == 0 {
		return
	}

	// Run a goroutine that will print the heartbeat on stdout.
	// Use a separate context that gets canceled upon function exit.
	heartbeatContext, cancel := context.WithCancel(context.Background())
	q.cancel = cancel

	q.wg.Add(1)
	go func(ctx context.Context) {
		defer q.wg.Done()
		ticker := time.Tick(q.heartbeatPeriod)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker:
				fmt.Fprintf(q.writer(), ".")
			}
		}
	}(heartbeatContext)
}

func (q *quietSpinner) Stop(error) {
	if q.cancel != nil {
		q.cancel()
		q.wg.Wait()
	}
}

func (q *quietSpinner) Done(string, ...any) {
	if q.cancel != nil {
		q.cancel()
		q.wg.Wait()
	}
}
