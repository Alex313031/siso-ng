// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type logSpinner struct {
	l       LogUI
	started time.Time
}

// Start implements the ui.spinner interface.
// Because a log-based UI cannot support an animated spinner, this is used only to report spinner completion.
func (l *logSpinner) Start(format string, args ...any) {
	l.started = time.Now()
	fmt.Fprintf(l.l.stdoutWriter(), format, args...)
}

// Stop implements the ui.spinner interface.
// Because a log-based UI cannot support an animated spinner, this is used to report how long the spinner operation took to complete.
func (l *logSpinner) Stop(err error) {
	if err != nil {
		fmt.Fprintf(l.l.stdoutWriter(), " failed %s %v\n", time.Since(l.started), err)
		return
	}
	fmt.Fprintf(l.l.stdoutWriter(), " done %s\n", time.Since(l.started))
}

// Done finishes the spinner with message.
func (l *logSpinner) Done(format string, args ...any) {
	fmt.Fprintf(l.l.stdoutWriter(), " %s %s\n", fmt.Sprintf(format, args...), time.Since(l.started))
}

// LogUI is a log-based UI.
type LogUI struct {
	Stdout, Stderr io.Writer
}

func (l LogUI) stdoutWriter() io.Writer {
	if l.Stdout != nil {
		return l.Stdout
	}
	return os.Stdout
}

func (l LogUI) stderrWriter() io.Writer {
	if l.Stderr != nil {
		return l.Stderr
	}
	return os.Stderr
}

// PrintLines implements the ui.ui interface.
// Because a log-based UI cannot support erasing previous lines, msgs will be printed as-is.
func (l LogUI) PrintLines(msgs ...string) {
	for i := range msgs {
		msgs[i] = StripANSIEscapeCodes(msgs[i])
	}
	l.stdoutWriter().Write([]byte(strings.Join(msgs, "\t") + "\n"))
}

// NewSpinner returns an implementation of ui.spinner.
func (l LogUI) NewSpinner() Spinner {
	return &logSpinner{l: l}
}

// Printf prints to stdout, stripping ansi escape sequence.
func (l LogUI) Printf(format string, args ...any) {
	fmt.Fprintf(l.stdoutWriter(), "%s", StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}

// Infof reports to stderr, stripping ansi escape sequence.
func (l LogUI) Infof(format string, args ...any) {
	fmt.Fprintf(l.stderrWriter(), "%s", StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}

// Warningf reports to stderr, stripping ansi escape sequence.
func (l LogUI) Warningf(format string, args ...any) {
	fmt.Fprintf(l.stderrWriter(), "%s", StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}

// Errorf reports to stderr, stripping ansi escape sequence.
func (l LogUI) Errorf(format string, args ...any) {
	fmt.Fprintf(l.stderrWriter(), "%s", StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}
