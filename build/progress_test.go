// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"testing"
	"time"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/ui"
)

func TestProgress_NotIsTerminal(t *testing.T) {
	currentUI := ui.Default
	defer func() { ui.Default = currentUI }()
	ui.Default = &ui.LogUI{}
	var p progress
	b := &Builder{
		statusReporter: noopStatusReporter{},
		plan:           &plan{},
		stats:          &stats{},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.start(ctx, b)

	step := &Step{
		cmd: &execute.Cmd{
			Desc: "ACTION sample",
		},
		state: &stepState{},
	}
	step.setPhase(stepStart)
	p.step(b, step, progressPrefixStart)
	started := time.Now()
	var count int64
	for count == 0 && time.Since(started) < 1*time.Second {
		time.Sleep(200 * time.Millisecond)
		count = p.count.Load()
		t.Logf("count=%d", count)
	}
	if count == 0 {
		t.Errorf("progress count=%d; want >0", count)
	}
	step.setPhase(stepDone)
	p.step(b, step, progressPrefixFinish)
	p.stop()

	if w := step.getWeightedDuration(); w == 0 {
		t.Errorf("weighted_duration=0; want non-zero")
	}
}
