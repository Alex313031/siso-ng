// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/resultstore"
	"go.chromium.org/build/siso/ui"
)

// activeItems is the number of running steps shown below the summary
// line on terminal progress. The tick renders a fixed frame of this
// many rows (padded if fewer steps are running) so PrintLines can
// redraw the same region in place.
const activeItems = 5

type progress struct {
	started    time.Time
	verbose    bool
	numLocal   atomic.Int32
	mu         sync.Mutex
	ts         time.Time
	consoleCmd *execute.Cmd

	// rendered is set true after the first render() has drawn the frame.
	// The first draw appends below the existing output (startup log,
	// spinner results, build-start message); subsequent draws redraw in
	// place. Written by render() on the update goroutine and reset by
	// stop() on the builder goroutine after the update goroutine has
	// returned (synchronized via <-p.updateStopped). All accesses are
	// sequential, so no mutex is needed.
	rendered bool

	// topActives holds up to activeItems oldest live steps, sorted
	// ascending by startTime. update() populates it unsorted as part
	// of the filter pass, then sorts it once before render() reads
	// it. Owned by the update goroutine so no mutex is needed.
	topActives []*stepInfo

	// topActivesNewest is the index in p.topActives of the kept entry
	// with the largest startTime while update() is populating the K
	// entry buffer. It gives insertTopActive a single comparison
	// rejection path in the common case where the candidate is newer
	// than every kept entry. Owned by the update goroutine so no
	// mutex is needed. The n == 0 branch in insertTopActive restores
	// it on the first call of each tick, so no explicit reset is
	// needed when topActives is cleared above.
	topActivesNewest int

	// linesBuf is the reusable backing for the terminal frame. start()
	// pre-sizes it to fit an optional leading "\n", one summary row,
	// activeItems step or pad rows, and a trailing blank row. render()
	// truncates via [:0] each tick and never grows beyond the initial
	// capacity, so no save-back is needed. Owned by the update goroutine.
	linesBuf []string

	// pendingOutput holds finished-step output (failure banners and
	// success-with-warnings) captured by step() in the terminal frame
	// path. The next render tick clears the frame, prints each entry
	// so it scrolls into history, then redraws the frame fresh below.
	// Guarded by mu; appended by worker goroutines, drained by the
	// update goroutine.
	pendingOutput []string

	resultstoreUploader *resultstore.Uploader

	actives       []*stepInfo
	done          chan struct{}
	stopOnce      sync.Once
	updateStopped chan struct{}
	count         atomic.Int64
}

type stepInfo struct {
	step *Step
	desc string
}

func (p *progress) start(ctx context.Context, b *Builder) {
	p.started = time.Now()
	p.verbose = b.verbose
	p.resultstoreUploader = b.resultstoreUploader
	p.done = make(chan struct{})
	p.updateStopped = make(chan struct{})
	p.linesBuf = make([]string, 0, activeItems+3)
	go p.update(ctx, b)
}

func (p *progress) startConsoleCmd(cmd *execute.Cmd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cmd.ConsoleOut = new(atomic.Bool)
	p.consoleCmd = cmd
}

func (p *progress) finishConsoleCmd() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consoleCmd = nil
}

func (p *progress) update(ctx context.Context, b *Builder) {
	lastUpdate := time.Now()
	defer close(p.updateStopped)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.count.Add(1)
			p.mu.Lock()
			// Drop every done step from p.actives. A done step left in
			// the slice would otherwise inflate the divisor for the
			// weighted duration below (addWeightedDuration drops the
			// share that lands on a done step). Fuse the top activeItems
			// oldest live steps into p.topActives during the same pass,
			// then sort that K entry buffer once so render() can emit it
			// without its own O(n log n) sort over p.actives.
			for i := range p.topActives {
				p.topActives[i] = nil
			}
			p.topActives = p.topActives[:0]
			n := 0
			for _, s := range p.actives {
				if s.step.Done() {
					continue
				}
				p.actives[n] = s
				n++
				p.insertTopActive(s)
			}
			for i := n; i < len(p.actives); i++ {
				p.actives[i] = nil
			}
			p.actives = p.actives[:n]
			sort.Slice(p.topActives, func(i, j int) bool {
				return p.topActives[i].step.startTime.Before(p.topActives[j].step.startTime)
			})
			if len(p.actives) > 0 {
				d := time.Since(lastUpdate)
				wd := d / time.Duration(len(p.actives))
				lastUpdate = time.Now()
				for _, s := range p.actives {
					s.step.addWeightedDuration(wd)
				}
			}
			p.mu.Unlock()
			p.render(b)
		}
	}
}

// insertTopActive keeps p.topActives as the activeItems oldest live
// steps seen so far this tick. Slots are written in the order the
// buffer filled; update() sorts the K entry buffer once after the
// p.actives walk so render() sees the K oldest in age order.
//
// p.topActivesNewest tracks the slot holding the newest of the kept
// K across calls, so the common case (candidate newer than every
// kept entry, dropped) is a single comparison. On the rare path
// where a candidate is strictly older than that slot, it replaces
// the slot and one linear scan over K slots picks the new newest.
// K is activeItems (5), so per call work is O(1) amortized with no
// allocation.
func (p *progress) insertTopActive(s *stepInfo) {
	switch {
	case len(p.topActives) < activeItems:
		// We have empty slots
		p.topActives = append(p.topActives, s)
	case s.step.startTime.Before(p.topActives[p.topActivesNewest].step.startTime):
		// s started before current newest so replace
		p.topActives[p.topActivesNewest] = s
	default:
		// s started later than current newest so ignore
		return
	}
	// Find newest
	p.topActivesNewest = 0
	for i := range p.topActives {
		if p.topActives[i].step.startTime.After(p.topActives[p.topActivesNewest].step.startTime) {
			p.topActivesNewest = i
		}
	}
}

// render draws the terminal progress frame: one summary line followed
// by up to activeItems step lines, each showing that step's
// servDuration, phase, and description. The frame size is fixed so
// PrintLines can redraw the same region in place. Non terminal and
// verbose modes do not render a frame; events handle their own output
// through progress.step.
func (p *progress) render(b *Builder) {
	if !ui.IsTerminal() || p.verbose {
		return
	}

	p.mu.Lock()
	if p.consoleCmd != nil && p.consoleCmd.ConsoleOut != nil && p.consoleCmd.ConsoleOut.Load() {
		p.mu.Unlock()
		return
	}
	pending := p.pendingOutput
	p.pendingOutput = nil
	p.mu.Unlock()
	// p.topActives was populated by the update tick immediately before
	// this call, sorted ascending by startTime and capped at activeItems.
	snapshot := p.topActives

	summary := p.buildSummary(b)
	lines := p.appendFrame(p.linesBuf[:0], summary, snapshot, pending)
	ui.Default.PrintLines(lines...)

	p.mu.Lock()
	p.ts = time.Now()
	p.mu.Unlock()
}

// buildSummary formats the one-line summary at the top of the terminal
// frame. It also updates p.numLocal (read by run_remote to gate
// fastLocal acquisition) and reports the plan's live total to
// b.statusReporter as side effects.
func (p *progress) buildSummary(b *Builder) string {
	dur := ui.FormatDuration(time.Since(b.start))
	stat := b.stats.stats()

	runProgress := func(waits, servs int) string {
		if waits > 0 {
			return ui.SGR(ui.BackgroundRed, fmt.Sprintf("%d", waits+servs))
		}
		return fmt.Sprintf("%d", servs)
	}
	preprocProgress := runProgress(b.preprocSema.NumWaits(), b.preprocSema.NumServs())

	localWaits := b.localSema.NumWaits()
	localServs := b.localSema.NumServs()
	for _, pool := range b.poolSemas {
		localWaits += pool.NumWaits()
		localServs += pool.NumServs()
	}
	// no wait for fastLocalSema since it only use TryAcquire,
	// so no need to count b.fastLocalSema.NumWaits.
	// fastLocal step also acquires localSema,
	// so no need to count b.fastLocalSema.NumServ.
	p.numLocal.Store(int32(localWaits + localServs))
	localProgress := runProgress(localWaits, localServs)

	remoteWaits := b.remoteSema.NumWaits() + b.reproxySema.NumWaits() + b.rewrapSema.NumWaits()
	remoteServs := b.remoteSema.NumServs() + b.reproxySema.NumServs() + b.rewrapSema.NumServs()
	remoteProgress := runProgress(remoteWaits, remoteServs)

	flushProgress := runProgress(hashfs.FlushSemaphore.NumWaits(), hashfs.FlushSemaphore.NumServs())

	var stepsPerSec string
	if stat.Done-stat.Skipped > 0 {
		stepsPerSec = fmt.Sprintf("%.1f/s ", float64(stat.Done-stat.Skipped)/time.Since(p.started).Seconds())
	}
	var cacheHitRatio string
	if stat.Remote+stat.CacheHit > 0 {
		// Use floor to avoid rounding up to 100% when there are cache misses.
		cacheHitRatio = fmt.Sprintf("cache:%5.02f%% ", math.Floor(float64(stat.CacheHit)/float64(stat.CacheHit+stat.Remote)*10000)/100.0)
	}
	var fallback string
	if stat.LocalFallback > 0 {
		fallback = "fallback:" + ui.SGR(ui.BackgroundRed, fmt.Sprintf("%d", stat.LocalFallback)) + " "
	}
	var cacheWrite string
	if stat.CacheWrite > 0 {
		cacheWrite = fmt.Sprintf("cache-write:%d(err:%d) ", stat.CacheWrite, stat.CacheWriteErr)
	}
	var retry string
	if stat.RemoteRetry > 0 {
		retry = "retry:" + ui.SGR(ui.BackgroundRed, fmt.Sprintf("%d", stat.RemoteRetry)) + " "
	}
	b.statusReporter.PlanHasTotalSteps(stat.Total - stat.Skipped)

	return fmt.Sprintf("[%d/%d] %s pre:%s local:%s remote:%s fetch:%s %s%s%s%s%s",
		stat.Done-stat.Skipped, stat.Total-stat.Skipped,
		dur,
		preprocProgress, localProgress, remoteProgress, flushProgress,
		stepsPerSec, cacheHitRatio, cacheWrite, fallback, retry,
	)
}

// appendFrame builds the terminal frame into lines and returns the
// extended slice. Normal redraws deliberately omit the leading "\n"
// sentinel so TermUI.PrintLines combines "clear old frame" and "write
// new frame" into one stdout write; Windows consoles visibly flicker
// if the frame is first cleared in a separate write.
//
// First renders and renders after pending output still use the sentinel
// because appendFrame has already positioned the cursor and PrintLines
// must not clear rows above the frame.
//
// Layout when the caller already owns the cursor:
//
//	"\n" sentinel telling PrintLines to skip its built-in clear path
//	summary
//	activeItems step or pad rows
//	trailing "\n" so the cursor lands on a blank row below the frame
//
// Reconciles the cursor before writing and drains pending into
// scrollback above the frame as a side effect.
func (p *progress) appendFrame(lines []string, summary string, snapshot []*stepInfo, pending []string) []string {
	// Position the cursor for cases where PrintLines must not do the
	// clear itself:
	//   - pending output: wipe the old frame, print the output into
	//     scrollback, then draw a fresh frame below it
	//   - first draw: the cursor may still be mid-line after startup
	//     text (build-start message, spinner result), so push past it
	//
	// The common no-pending redraw intentionally does nothing here.
	// PrintLines will clear the previous fixed-height frame and write
	// the replacement frame in one buffer, which avoids a visible blank
	// frame on Windows.
	if p.rendered && len(pending) > 0 {
		p.clearFrame()
	} else if !p.rendered {
		ui.Default.Printf("\n")
	}

	// Drain queued step output into scrollback above the new frame.
	// Pending must be flushed after the clear/push above and before
	// the frame, so it scrolls into history rather than being
	// overwritten by the frame.
	if len(pending) > 0 {
		p.writePending(pending)
	}

	// Leading "\n" is the sentinel that tells PrintLines we already
	// own the cursor. Omit it for normal redraws so PrintLines clears
	// and writes the frame in one stdout write.
	if !p.rendered || len(pending) > 0 {
		lines = append(lines, "\n")
	}
	lines = append(lines, summary)
	for _, si := range snapshot {
		lines = append(lines, formatStepRow(si))
	}

	// Pad the step region to a fixed height with " " (not "") because
	// writeLinesMaxWidth skips empty msgs, which would desync the
	// clear and write line counts.
	for range activeItems - len(snapshot) {
		lines = append(lines, " ")
	}

	lines = append(lines, "\n")
	p.rendered = true
	return lines
}

// formatStepRow formats one active-step row in the terminal frame:
// servDuration (or 6 spaces if zero), phase, and description. Rows for
// fallback or retry phases are colored red.
func formatStepRow(si *stepInfo) string {
	phase := si.step.phase()
	d := si.step.servDuration()
	durStr := "      "
	if d > 0 {
		durStr = fmt.Sprintf("%6s", ui.FormatDuration(d))
	}
	msg := fmt.Sprintf("  %s [%s] %s", durStr, phase, si.desc)
	switch phase {
	case stepFallbackWait, stepFallbackRun, stepRetryWait, stepRetryRun:
		msg = ui.SGR(ui.Red, msg)
	}
	return msg
}

func (p *progress) stop() {
	p.stopOnce.Do(func() {
		close(p.done)
	})
	<-p.updateStopped
	// Drain output queued after the last render tick (a failure in the
	// final 100ms would otherwise be lost). Wipe the frame still on
	// screen, print the queue, and mark rendered=false so the caller's
	// follow-up clearFrame is a no op and its "<name> finished/failed"
	// PrintLines lands on the empty line below.
	p.mu.Lock()
	pending := p.pendingOutput
	p.pendingOutput = nil
	p.mu.Unlock()
	if len(pending) > 0 {
		p.clearFrame()
		p.writePending(pending)
		p.rendered = false
	}
}

// enqueueOutput routes a one-off message (e.g. "last failed target
// fixed") through the same pending queue as step output so it scrolls
// into history above the frame instead of clobbering it. In non
// terminal or verbose mode there is no frame, so it prints directly.
func (p *progress) enqueueOutput(msg string) {
	if !ui.IsTerminal() || p.verbose {
		ui.Default.PrintLines(msg)
		return
	}
	p.mu.Lock()
	p.pendingOutput = append(p.pendingOutput, msg)
	p.mu.Unlock()
}

// writePending writes already-drained pending output to stdout, each
// entry terminated by "\n" so the cursor lands on a fresh blank line
// after the last write. Caller is responsible for clearing the frame
// first (if any) and for whatever follows the cursor.
func (p *progress) writePending(pending []string) {
	var buf strings.Builder
	for _, msg := range pending {
		buf.WriteString(msg)
		if !strings.HasSuffix(msg, "\n") {
			buf.WriteByte('\n')
		}
	}
	ui.Default.Printf("%s", buf.String())
}

// clearFrame erases the activeItems+2 rows of the terminal frame last
// drawn by render (the summary line, activeItems step or pad rows, and
// the trailing blank row), leaving the cursor at the top of that
// region so the caller can overwrite it with a final message. No op
// when a terminal frame was never drawn (non terminal UI, verbose
// mode, or no prior render). Not safe to call while the tick goroutine
// is still running; the caller must stop() first.
func (p *progress) clearFrame() {
	if !ui.IsTerminal() || p.verbose || !p.rendered {
		return
	}
	ui.Default.PrintLines(make([]string, activeItems+2)...)
}

func (p *progress) report(format string, args ...any) {
	p.mu.Lock()
	t := p.ts
	p.mu.Unlock()
	var msg string
	if p.resultstoreUploader != nil {
		msg = fmt.Sprintf(format, args...)
		p.resultstoreUploader.AddBuildLog(msg + "\n")
	}
	if ui.IsTerminal() && time.Since(t) < 500*time.Millisecond {
		return
	}
	if msg == "" {
		msg = fmt.Sprintf(format, args...)
	}
	ui.Default.PrintLines(msg)
	p.mu.Lock()
	p.ts = time.Now()
	p.mu.Unlock()
}

const (
	progressPrefixCacheHit   = "c "
	progressPrefixStart      = "S "
	progressPrefixFinish     = "F "
	progressPrefixError      = "E "
	progressPrefixCanceled   = "- "
	progressPrefixCacheWrite = "W "
	progressPrefixRetry      = "r "
	progressPrefixFallback   = "f "
)

func (p *progress) step(b *Builder, step *Step, s string) {
	p.mu.Lock()
	if step != nil {
		if strings.HasPrefix(s, progressPrefixStart) {
			p.actives = append(p.actives, &stepInfo{
				step: step,
				desc: step.cmd.Desc,
			})
		}
	}
	p.mu.Unlock()
	dur := ui.FormatDuration(time.Since(b.start))
	stat := b.stats.stats()
	if step != nil && p.resultstoreUploader != nil {
		switch {
		case strings.HasPrefix(s, progressPrefixStart),
			strings.HasPrefix(s, progressPrefixFinish),
			strings.HasPrefix(s, progressPrefixError),
			strings.HasPrefix(s, progressPrefixCacheHit):
			p.resultstoreUploader.AddBuildLog(fmt.Sprintf("[%d/%d] %s %s\n",
				stat.Done-stat.Skipped, stat.Total-stat.Skipped,
				dur,
				s))
		}
	}
	var outputResult string
	if (strings.HasPrefix(s, progressPrefixFinish) || strings.HasPrefix(s, progressPrefixError)) && step != nil {
		outputResult = step.cmd.OutputResult()
	}
	if ui.IsTerminal() && !p.verbose {
		// The tick-driven render() owns the terminal frame. Step
		// output (failure banners, success-with-warnings) is queued
		// here and printed by the next render tick above the redrawn
		// frame so it scrolls into history. The full per-step record
		// still goes to the output log via run_step.logOutput.
		if outputResult != "" {
			msg := fmt.Sprintf("[%d/%d] %s %s\n%s",
				stat.Done-stat.Skipped, stat.Total-stat.Skipped,
				dur, s, outputResult)
			p.mu.Lock()
			p.pendingOutput = append(p.pendingOutput, msg)
			p.mu.Unlock()
		}
		return
	}
	// From here: verbose mode, or non terminal UI (LogUI).
	msg := s
	if p.verbose {
		if strings.HasPrefix(s, progressPrefixStart) && step != nil {
			msg = fmt.Sprintf("[%d/%d] %s %s",
				stat.Done-stat.Skipped, stat.Total-stat.Skipped,
				dur,
				step.def.Binding("command"))
			ui.Default.Printf("%s\n", msg)
		} else if (strings.HasPrefix(s, progressPrefixFinish) || strings.HasPrefix(s, progressPrefixError)) && step != nil {
			msg = fmt.Sprintf("[%d/%d] %s %s",
				stat.Done-stat.Skipped, stat.Total-stat.Skipped,
				dur,
				step.def.Binding("command"))
			if outputResult != "" {
				msg += "\n" + outputResult + "\n"
			}
			ui.Default.Printf("%s\n", msg)
		} else if step == nil {
			ui.Default.Printf("%s\n", msg)
		}
	} else {
		var lines []string
		if step != nil {
			b.statusReporter.PlanHasTotalSteps(stat.Total - stat.Skipped)
			msg = fmt.Sprintf("[%d/%d] %s %s",
				stat.Done-stat.Skipped, stat.Total-stat.Skipped,
				dur,
				s)
			if outputResult != "" {
				outputResult = "\n" + outputResult
			}
		}
		lines = append(lines, msg)
		if outputResult != "" {
			lines = append(lines, outputResult+"\n")
		}
		ui.Default.PrintLines(lines...)
	}
	p.mu.Lock()
	p.ts = time.Now()
	p.mu.Unlock()
}

type ActiveStepInfo struct {
	ID    string
	Desc  string
	Phase string

	// step duration since when it's ready to run.
	Dur string

	// step accumulated service duration (local exec, or remote call).
	// not including local execution queue, remote execution queue,
	// but including all of remote call (reapi or reproxy), uploading
	// inputs, downloading outputs etc.
	ServDur string
}

func (p *progress) ActiveSteps() []ActiveStepInfo {
	p.mu.Lock()
	actives := make([]*stepInfo, len(p.actives))
	copy(actives, p.actives)
	p.mu.Unlock()
	now := time.Now()
	as := actives[:0]
	for _, s := range actives {
		if s.step.phase().String() == "done" {
			continue
		}
		as = append(as, s)
	}
	sort.Slice(as, func(i, j int) bool {
		return as[i].step.startTime.Before(as[j].step.startTime)
	})

	activeSteps := make([]ActiveStepInfo, 0, len(as))
	for _, s := range as {
		var servDur string
		if dur := s.step.servDuration(); dur > 0 {
			servDur = ui.FormatDuration(dur)
		}
		activeSteps = append(activeSteps, ActiveStepInfo{
			ID:      s.step.String(),
			Desc:    s.desc,
			Phase:   s.step.phase().String(),
			Dur:     ui.FormatDuration(now.Sub(s.step.startTime)),
			ServDur: servDur,
		})
	}
	return activeSteps
}
