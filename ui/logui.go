// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type logSpinner struct {
	started time.Time
}

// Start implements the ui.spinner interface.
// Because a log-based UI cannot support an animated spinner, this is used only to report spinner completion.
func (l *logSpinner) Start(format string, args ...any) {
	l.started = time.Now()
	fmt.Printf(format, args...)
}

// Stop implements the ui.spinner interface.
// Because a log-based UI cannot support an animated spinner, this is used to report how long the spinner operation took to complete.
func (l *logSpinner) Stop(err error) {
	if err != nil {
		fmt.Printf(" failed %s %v\n", time.Since(l.started), err)
		return
	}
	fmt.Printf(" done %s\n", time.Since(l.started))
}

// Done finishes the spinner with message.
func (l *logSpinner) Done(format string, args ...any) {
	fmt.Printf(" %s %s\n", fmt.Sprintf(format, args...), time.Since(l.started))
}

// LogUI is a log-based UI.
type LogUI struct{}

// PrintLines implements the ui.ui interface.
// Because a log-based UI cannot support erasing previous lines, msgs will be printed as-is.
func (LogUI) PrintLines(msgs ...string) {
	for i := range msgs {
		msgs[i] = StripANSIEscapeCodes(msgs[i])
	}
	os.Stdout.Write([]byte(strings.Join(msgs, "\t") + "\n"))
}

// NewSpinner returns an implementation of ui.spinner.
func (LogUI) NewSpinner() Spinner {
	return &logSpinner{}
}

// Infof reports to stdout, stripping ansi escape sequence.
func (LogUI) Infof(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "%s", StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}

// Warningf reports to stderr, stripping ansi escape sequence.
func (LogUI) Warningf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s", StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}

// Errorf reports to stderr, stripping ansi escape sequence.
func (LogUI) Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s", StripANSIEscapeCodes(fmt.Sprintf(format, args...)))
}
