// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/ui"
)

type recordingUI struct {
	printed    []string
	printLines [][]string
}

func (r *recordingUI) PrintLines(msgs ...string) {
	r.printLines = append(r.printLines, slices.Clone(msgs))
}

func (r *recordingUI) NewSpinner() ui.Spinner {
	return noopSpinner{}
}

func (r *recordingUI) Printf(format string, args ...any) {
	r.printed = append(r.printed, fmt.Sprintf(format, args...))
}

func (r *recordingUI) Infof(format string, args ...any) {
	r.Printf(format, args...)
}

func (r *recordingUI) Warningf(format string, args ...any) {
	r.Printf(format, args...)
}

func (r *recordingUI) Errorf(format string, args ...any) {
	r.Printf(format, args...)
}

type noopSpinner struct{}

func (noopSpinner) Start(format string, args ...any) {}
func (noopSpinner) Stop(err error)                   {}
func (noopSpinner) Done(format string, args ...any)  {}

// newProgressTestSteps builds three steps in started state with
// staggered startTimes, suitable for driving the update goroutine
// under synctest.
func newProgressTestSteps() (s0, s1, s2 *Step) {
	now := time.Now()
	mkStep := func(desc string, start time.Time) *Step {
		s := &Step{
			cmd:       &execute.Cmd{Desc: desc},
			state:     &stepState{},
			startTime: start,
		}
		s.setPhase(stepStart)
		return s
	}
	s0 = mkStep("head", now.Add(-3*time.Second))
	s1 = mkStep("middle", now.Add(-2*time.Second))
	s2 = mkStep("tail", now.Add(-1*time.Second))
	return
}

func TestProgressAppendFrame_NormalRedrawLetsPrintLinesClear(t *testing.T) {
	currentUI := ui.Default
	rec := &recordingUI{}
	ui.Default = rec
	defer func() { ui.Default = currentUI }()

	p := progress{rendered: true}
	got := p.appendFrame(nil, "summary", nil, nil)

	if len(got) == 0 || got[0] == "\n" {
		t.Fatalf("appendFrame returned leading sentinel for normal redraw: %q", got)
	}
	if want := activeItems + 2; len(got) != want {
		t.Fatalf("appendFrame returned %d lines; want %d", len(got), want)
	}
	if len(rec.printed) != 0 {
		t.Fatalf("appendFrame performed separate stdout writes %q; want none on normal redraw", rec.printed)
	}
	if !p.rendered {
		t.Fatal("appendFrame cleared rendered state; want rendered frame to remain active")
	}
}

func TestProgressAppendFrame_FirstRenderOwnsCursor(t *testing.T) {
	currentUI := ui.Default
	rec := &recordingUI{}
	ui.Default = rec
	defer func() { ui.Default = currentUI }()

	var p progress
	got := p.appendFrame(nil, "summary", nil, nil)

	if len(got) == 0 || got[0] != "\n" {
		t.Fatalf("appendFrame first render lines=%q; want leading sentinel", got)
	}
	if want := activeItems + 3; len(got) != want {
		t.Fatalf("appendFrame first render returned %d lines; want %d", len(got), want)
	}
	if !slices.Equal(rec.printed, []string{"\n"}) {
		t.Fatalf("appendFrame first render writes=%q; want newline cursor push", rec.printed)
	}
	if !p.rendered {
		t.Fatal("appendFrame did not mark first render as active")
	}
}

func TestProgressAppendFrame_PendingOutputOwnsCursor(t *testing.T) {
	currentUI := ui.Default
	rec := &recordingUI{}
	ui.Default = rec
	defer func() { ui.Default = currentUI }()

	p := progress{rendered: true}
	got := p.appendFrame(nil, "summary", nil, []string{"finished with warning"})

	if len(got) == 0 || got[0] != "\n" {
		t.Fatalf("appendFrame pending-output lines=%q; want leading sentinel", got)
	}
	if want := activeItems + 3; len(got) != want {
		t.Fatalf("appendFrame pending-output returned %d lines; want %d", len(got), want)
	}
	if !slices.Equal(rec.printed, []string{"finished with warning\n"}) {
		t.Fatalf("appendFrame pending-output writes=%q; want queued output before redraw", rec.printed)
	}
	if !p.rendered {
		t.Fatal("appendFrame cleared rendered state; want rendered frame to remain active")
	}
}

// TestProgress_DoneMiddleStepRemovedFromActives checks that update()
// removes a done step from the middle of p.actives, not only from the
// head.
func TestProgress_DoneMiddleStepRemovedFromActives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		currentUI := ui.Default
		defer func() { ui.Default = currentUI }()
		ui.Default = &ui.LogUI{}

		var p progress
		b := &Builder{
			statusReporter: noopStatusReporter{},
			plan:           &plan{},
			stats:          &stats{},
		}

		s0, s1, s2 := newProgressTestSteps()

		p.start(t.Context(), b)
		t.Cleanup(p.stop)

		p.step(b, s0, progressPrefixStart+s0.cmd.Desc)
		p.step(b, s1, progressPrefixStart+s1.cmd.Desc)
		p.step(b, s2, progressPrefixStart+s2.cmd.Desc)
		s1.setPhase(stepDone)

		// Advance virtual time past one tick (ticker is 100ms).
		// synctest intercepts Sleep and advances fake time only when
		// all bubble goroutines are durably blocked, so by the time
		// Sleep returns the tick has fired and update() is parked on
		// the next ticker receive.
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()

		p.mu.Lock()
		got := len(p.actives)
		p.mu.Unlock()

		if want := 2; got != want {
			t.Errorf("len(p.actives)=%d after middle step finished; want %d", got, want)
		}
	})
}

// TestProgress_WeightedDurationExcludesDoneStep checks that update()
// divides the tick duration across live steps only.
func TestProgress_WeightedDurationExcludesDoneStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		currentUI := ui.Default
		defer func() { ui.Default = currentUI }()
		ui.Default = &ui.LogUI{}

		var p progress
		b := &Builder{
			statusReporter: noopStatusReporter{},
			plan:           &plan{},
			stats:          &stats{},
		}

		s0, s1, s2 := newProgressTestSteps()

		p.start(t.Context(), b)
		t.Cleanup(p.stop)

		p.step(b, s0, progressPrefixStart+s0.cmd.Desc)
		p.step(b, s1, progressPrefixStart+s1.cmd.Desc)
		p.step(b, s2, progressPrefixStart+s2.cmd.Desc)
		s1.setPhase(stepDone)

		// Let the first tick run the done sweep so only s0 and s2
		// remain in actives when the next tick accounts weight.
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()

		w0Before := s0.getWeightedDuration()
		w2Before := s2.getWeightedDuration()

		// Advance one more full tick. With 2 live steps, each should
		// receive d/2 = 50ms of weight.
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		w0 := s0.getWeightedDuration() - w0Before
		w2 := s2.getWeightedDuration() - w2Before

		// Under synctest virtual time, Sleep is exact and the ticker
		// d is deterministically 100ms. Allow 1ms slack for the
		// time.Since measurement inside update() crossing a tick.
		want := 50 * time.Millisecond
		slack := 1 * time.Millisecond
		if w0 < want-slack || w0 > want+slack {
			t.Errorf("s0 weight gained=%v in one tick; want %v (+-%v)", w0, want, slack)
		}
		if w2 < want-slack || w2 > want+slack {
			t.Errorf("s2 weight gained=%v in one tick; want %v (+-%v)", w2, want, slack)
		}
		if w1 := s1.getWeightedDuration(); w1 != 0 {
			t.Errorf("s1 (done) weight=%v; want 0", w1)
		}
	})
}

// mkTopActive is a helper that returns a stepInfo whose startTime is
// offset seconds from a fixed epoch. Only step.startTime is read by
// insertTopActive, so no other Step fields need to be populated.
func mkTopActive(offsetSec int) *stepInfo {
	base := time.Unix(1_700_000_000, 0)
	return &stepInfo{
		step: &Step{startTime: base.Add(time.Duration(offsetSec) * time.Second)},
	}
}

// topActiveOffsets returns the startTime offsets (seconds past epoch)
// for every entry in p.topActives, so tests can compare against a
// plain []int literal.
func topActiveOffsets(p *progress) []int {
	base := time.Unix(1_700_000_000, 0)
	out := make([]int, len(p.topActives))
	for i, s := range p.topActives {
		out[i] = int(s.step.startTime.Sub(base) / time.Second)
	}
	return out
}

// sortTopActives applies the same ordering update() performs at the
// end of its filter pass, so per insert tests can assert the K
// oldest in the order render() sees.
func sortTopActives(p *progress) {
	slices.SortFunc(p.topActives, func(a, b *stepInfo) int {
		return a.step.startTime.Compare(b.step.startTime)
	})
}

// TestInsertTopActive_FillInReverseOrder inserts K candidates from
// newest to oldest while the buffer is below capacity. Each call
// appends; the update tick sorts the K entry buffer once after the
// walk, so the result is ascending by startTime.
func TestInsertTopActive_FillInReverseOrder(t *testing.T) {
	var p progress
	for offset := activeItems; offset >= 1; offset-- {
		p.insertTopActive(mkTopActive(offset))
	}
	sortTopActives(&p)
	want := []int{1, 2, 3, 4, 5}
	if got := topActiveOffsets(&p); !slices.Equal(got, want) {
		t.Errorf("topActives offsets=%v; want %v", got, want)
	}
}

// TestInsertTopActive_FillInOrder inserts K candidates from oldest to
// newest while the buffer is below capacity. Each call appends in
// order, and the update tick sort leaves the result ascending by
// startTime.
func TestInsertTopActive_FillInOrder(t *testing.T) {
	var p progress
	for offset := 1; offset <= activeItems; offset++ {
		p.insertTopActive(mkTopActive(offset))
	}
	sortTopActives(&p)
	want := []int{1, 2, 3, 4, 5}
	if got := topActiveOffsets(&p); !slices.Equal(got, want) {
		t.Errorf("topActives offsets=%v; want %v", got, want)
	}
}

// TestInsertTopActive_DoesNotExceedCap ensures the buffer never grows
// past activeItems no matter how many candidates are offered.
func TestInsertTopActive_DoesNotExceedCap(t *testing.T) {
	var p progress
	for offset := range activeItems * 3 {
		p.insertTopActive(mkTopActive(offset))
	}
	if got := len(p.topActives); got != activeItems {
		t.Errorf("len(topActives)=%d; want %d", got, activeItems)
	}
}

// TestInsertTopActive_EvictsNewestWhenFull checks that once the buffer
// is full, a candidate strictly older than the newest of the kept K
// replaces that slot, so after the update tick sort the K oldest are
// in ascending age order.
func TestInsertTopActive_EvictsNewestWhenFull(t *testing.T) {
	var p progress
	for _, off := range []int{10, 20, 30, 40, 50} {
		p.insertTopActive(mkTopActive(off))
	}
	// 25 is older than the newest of the kept K (50) and belongs
	// between 20 and 30 after the tick sort.
	p.insertTopActive(mkTopActive(25))
	sortTopActives(&p)
	want := []int{10, 20, 25, 30, 40}
	if got := topActiveOffsets(&p); !slices.Equal(got, want) {
		t.Errorf("topActives offsets=%v; want %v", got, want)
	}
}

// TestInsertTopActive_SkipsWhenNotOlder checks that once the buffer is
// full, a candidate no older than the newest of the kept K is dropped
// and the buffer is left unchanged.
func TestInsertTopActive_SkipsWhenNotOlder(t *testing.T) {
	var p progress
	for _, off := range []int{10, 20, 30, 40, 50} {
		p.insertTopActive(mkTopActive(off))
	}
	before := topActiveOffsets(&p)
	// 60 is newer than the newest of the kept K, must be skipped.
	p.insertTopActive(mkTopActive(60))
	// 50 ties the newest of the kept K, must also be skipped (strict
	// older rule).
	p.insertTopActive(mkTopActive(50))
	if got := topActiveOffsets(&p); !slices.Equal(got, before) {
		t.Errorf("topActives offsets=%v; want unchanged %v", got, before)
	}
}

// TestInsertTopActive_CandidateOlderThanAll offers a candidate older
// than every kept entry, so the slot holding the newest of the kept K
// is replaced and after the update tick sort the candidate leads.
func TestInsertTopActive_CandidateOlderThanAll(t *testing.T) {
	var p progress
	for _, off := range []int{10, 20, 30, 40, 50} {
		p.insertTopActive(mkTopActive(off))
	}
	p.insertTopActive(mkTopActive(1))
	sortTopActives(&p)
	want := []int{1, 10, 20, 30, 40}
	if got := topActiveOffsets(&p); !slices.Equal(got, want) {
		t.Errorf("topActives offsets=%v; want %v", got, want)
	}
}

// TestInsertTopActive_RandomOrderMatchesSort feeds a shuffled stream
// through insertTopActive, then applies the update tick sort, and
// checks that the result is identical to sorting all offsets and
// taking the first K entries (the K oldest).
func TestInsertTopActive_RandomOrderMatchesSort(t *testing.T) {
	const n = 200
	offsets := make([]int, n)
	for i := range offsets {
		offsets[i] = i
	}
	rng := rand.New(rand.NewPCG(1, 2))
	rng.Shuffle(n, func(i, j int) { offsets[i], offsets[j] = offsets[j], offsets[i] })

	var p progress
	for _, off := range offsets {
		p.insertTopActive(mkTopActive(off))
	}
	sortTopActives(&p)
	got := topActiveOffsets(&p)

	sorted := slices.Clone(offsets)
	slices.Sort(sorted)
	want := sorted[:activeItems]
	if !slices.Equal(got, want) {
		t.Errorf("topActives offsets=%v; want %v", got, want)
	}
}

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
