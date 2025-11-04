// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

type termSpinner struct {
	quit, done chan struct{}
	started    time.Time
	n          int
	msg        string
}

// Start starts the spinner.
func (s *termSpinner) Start(format string, args ...any) {
	s.started = time.Now()
	s.msg = fmt.Sprintf(format, args...)
	fmt.Printf("%s...", s.msg)
	s.quit = make(chan struct{})
	s.done = make(chan struct{})
	fmt.Printf(" ")
	go func() {
		defer close(s.done)
		for {
			select {
			case <-s.quit:
				return
			case <-time.After(1 * time.Second):
				const chars = `/-\|`
				fmt.Printf("\b%c", chars[s.n])
				s.n++
				if s.n >= len(chars) {
					s.n = 0
				}
			}
		}
	}()
}

// Stop stops the spinner.
func (s *termSpinner) Stop(err error) {
	close(s.quit)
	<-s.done
	d := time.Since(s.started)
	if err != nil {
		fmt.Printf("\r\033[K%6s %s failed %v\n", FormatDuration(d), s.msg, err)
		return
	}
	if d < DurationThreshold {
		// omit if duration is too short
		fmt.Printf("\r\033[K")
		return
	}
	fmt.Printf("\r\033[K%6s %s\n", FormatDuration(d), s.msg)
}

// Done finishes the spinner with message.
func (s *termSpinner) Done(format string, args ...any) {
	close(s.quit)
	<-s.done
	msg := fmt.Sprintf(format, args...)
	d := time.Since(s.started)
	fmt.Printf("\r\033[K%6s %s %s\n", FormatDuration(d), s.msg, msg)
}

// TermUI is a terminal-based UI.
type TermUI struct {
	noColor bool
	mu      sync.Mutex
	timer   time.Time
	height  int
	width   int
}

func (t *TermUI) update() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.timer) > 200*time.Millisecond {
		t.timer = time.Now()
		t.width, t.height, _ = term.GetSize(int(os.Stdout.Fd()))
	}
	return t.width, t.height
}

func (t *TermUI) Height() int {
	_, h := t.update()
	return h
}

func (t *TermUI) Width() int {
	w, _ := t.update()
	return w
}

func (t *TermUI) msg(s string) string {
	if t.noColor {
		return StripANSIEscapeCodes(s)
	}
	return s
}

// PrintLines implements the ui.ui interface.
// msg not including \n will fit on one terminal line, by eliding
// middle for long msg.
// msg including \n will print as is.
// If msgs starts with \n, it will print from the current line.
// Otherwise, it will replace the last N lines, where N is number of msgs
// that don't start with \n.
func (t *TermUI) PrintLines(msgs ...string) {
	var buf bytes.Buffer
	if len(msgs) > 0 && msgs[0] == "\n" {
		msgs = msgs[1:]
	} else {
		// Clear the last N lines, where N is number of msgs
		// that don't start with \n.
		for i := range len(msgs) - 1 {
			if msgs[i][0] == '\n' {
				msgs[i] = msgs[i][1:]
				break
			}
			fmt.Fprintf(&buf, "\r\033[K\033[A")
		}
		fmt.Fprintf(&buf, "\r\033[K")
	}
	for i := range msgs {
		msgs[i] = t.msg(msgs[i])
	}
	writeLinesMaxWidth(&buf, msgs, t.Width())
	os.Stdout.Write(buf.Bytes())
}

// NewSpinner returns a terminal-based spinner.
func (t *TermUI) NewSpinner() Spinner {
	return &termSpinner{}
}

// Infof reports to stdout.
func (t *TermUI) Infof(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "%s", t.msg(fmt.Sprintf(format, args...)))
}

// Warningf reports to stderr.
func (t *TermUI) Warningf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s", t.msg(fmt.Sprintf(format, args...)))
}

// Errorf reports to stderr.
func (t *TermUI) Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s", t.msg(fmt.Sprintf(format, args...)))
}
