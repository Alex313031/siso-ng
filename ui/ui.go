// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ui provides user interface functionalities.
package ui

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/term"
)

// Spinner controls spinner.
type Spinner interface {
	// Start starts the spinner with the specified formatted string.
	Start(format string, args ...any)
	// Stop stops the spinner, outputting an error if provided.
	Stop(err error)
	// Done finishes the spinner with message.
	Done(format string, args ...any)
}

// UI is a user interface.
type UI interface {
	// PrintLines prints message lines.
	// If msgs starts with \n, it will print from the current line.
	// Otherwise, it will replaces the last N lines, where N is len(msgs).
	PrintLines(msgs ...string)
	// NewSpinner returns a new spinner.
	NewSpinner() Spinner

	// Infof reports info level.
	Infof(string, ...any)

	// Warningf reports warning level.
	Warningf(string, ...any)

	// Errorf reports error level.
	Errorf(string, ...any)
}

// Default holds the default UI interface.
// Making changes to this variable after init is undefined behavior.
// UI implementations are currently not expected to handle being changed.
var Default UI

// termUI needs
// - cursor up : \033[A
// - kill line:  \033[K
// disable termUI if term doesn't support these feature.
// https://invisible-island.net/ncurses/terminfo.src.html
func isSmartTerminal(term string) bool {
	switch term {
	case "dumb", "dumb-emacs-ansi":
		return false
	}
	// assume default terminal works.
	return true
}

func init() {
	if isSmartTerminal(os.Getenv("TERM")) && term.IsTerminal(int(os.Stdout.Fd())) {
		termUI := &TermUI{
			noColor: os.Getenv("NO_COLOR") != "",
		}
		Default = termUI
	} else {
		Default = &LogUI{}
	}
}

// IsTerminal returns whether currently using a terminal UI.
func IsTerminal() bool {
	_, ok := Default.(*TermUI)
	return ok
}

func writeLinesMaxWidth(buf *bytes.Buffer, msgs []string, width int) {
	for i, msg := range msgs {
		if msg == "" {
			continue
		}
		// Truncate in middle if too long, unless terminated with newline.
		// If printing last message, don't truncate if any newline.
		if width > 4 && len(msg)+3 > width-1 && ((width-4)/2) < len(msg) &&
			((i < len(msgs)-1 && !strings.Contains(msg[:len(msg)-1], "\n")) ||
				(i == len(msgs)-1 && !strings.Contains(msg, "\n"))) {
			msg = elideMiddle(msg, width)
		}
		if i > 0 {
			fmt.Fprintln(buf)
		}
		msg = strings.TrimSuffix(msg, "\n")
		fmt.Fprint(buf, msg)
	}
}

func elideMiddle(msg string, width int) string {
	chrs := make([]byte, 0, len(msg))
	sgrs := make([]string, 0, len(msg))
	var sgr string
	hasSGR := false
	const escapeSeq = "\033["
	for i := 0; i < len(msg); i++ {
		if strings.HasPrefix(msg[i:], escapeSeq) {
			i += len(escapeSeq)
			j := strings.Index(msg[i:], "m")
			if j < 0 {
				// no SGR escape sequence?
				// TODO: handle this case correctly
				chrs = append(chrs, []byte(escapeSeq)...)
				chrs = append(chrs, []byte(msg[i:])...)
				hasSGR = false
				break
			}
			hasSGR = true
			sgr = msg[i : i+j]
			i += j
			continue
		}
		chrs = append(chrs, msg[i])
		sgrs = append(sgrs, sgr)
	}
	const elideMarker = "..."
	if len(chrs) < width {
		return msg
	}
	n := (width - (len(elideMarker) + 1)) / 2
	if len(chrs)+len(elideMarker) <= width-1 || n > len(chrs) {
		return msg
	}
	if !hasSGR {
		return msg[:n] + "..." + msg[len(msg)-n:]
	}
	sgr = ""
	var sb strings.Builder
	for i := range n {
		if sgrs[i] != sgr {
			sb.WriteString(escapeSeq)
			sb.WriteString(sgrs[i])
			sb.WriteString("m")
			sgr = sgrs[i]
		}
		sb.WriteByte(chrs[i])
	}
	if sgr != "" && sgr != "0" {
		sb.WriteString(escapeSeq + "0m")
	}
	sb.WriteString("...")
	sgr = "0"
	for i := len(chrs) - n; i < len(chrs); i++ {
		if sgrs[i] != sgr {
			sb.WriteString(escapeSeq)
			sb.WriteString(sgrs[i])
			sb.WriteString("m")
			sgr = sgrs[i]
		}
		sb.WriteByte(chrs[i])
	}
	if sgr != "" && sgr != "0" {
		sb.WriteString(escapeSeq + "0m")
	}
	return sb.String()
}

// https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_(Select_Graphic_Rendition)_parameters
type SGRCode int

const (
	Bold SGRCode = iota
	Red
	Green
	Blue
	Yellow
	BackgroundRed
	Reset
)

var sgrEscSeq = map[SGRCode]string{
	Bold:          "\033[1m",
	Red:           "\033[1,31m",
	Green:         "\033[32m",
	Blue:          "\033[96m",
	Yellow:        "\033[33m",
	BackgroundRed: "\033[41;37m",
	Reset:         "\033[0m",
}

func (s SGRCode) String() string {
	return sgrEscSeq[s]
}

// SGR formats s in SGR (select graphic rendition).
func SGR(n SGRCode, s string) string {
	return fmt.Sprintf("%s%s%s", n, s, Reset)
}

// StripANSIEscapeCodes strips ANSI escape codes.
func StripANSIEscapeCodes(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\033' {
			// not an escape code.
			sb.WriteByte(s[i])
			continue
		}
		// Only strip CSIs for now.
		if i+1 >= len(s) {
			break
		}
		if s[i+1] != '[' {
			// Not a CSI.
			continue
		}
		i += 2

		// Loop while current char is NOT an ASCII letter.
		for i < len(s) && !unicode.IsLetter(rune(s[i])) {
			i++
		}
	}
	return sb.String()
}
