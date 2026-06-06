// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"go.chromium.org/build/siso/ui"
)

// ninja compatible debug mode.
type debugMode struct {
	Stats       bool // print operation counts/timing info
	Explain     bool // explain what caused a command to execute
	Keepdepfile bool // don't delete depfiles after they're read by ninja
	Keeprsp     bool // don't delete @response files on success
	List        bool // lists modes
}

func (m *debugMode) check() error {
	if m.List {
		return errors.New(`debugging modes
  stats        not implemented: print operation counts/timing info
  explain      explain what caused a command to execute
  keepdepfile  don't delete depfiles after they're read by ninja
  keeprsp      don't delete @response files on success
multiple modes can be enabled via -d FOO -d BAR`)
	}
	if m.Stats {
		ui.Default.Warningf("WARNING: `-d stats` is not implemented yet\n")
	}
	return nil
}

// String returns mode flag value as comma separated values.
func (m *debugMode) String() string {
	var modes []string
	if m.Stats {
		modes = append(modes, "stats")
	}
	if m.Explain {
		modes = append(modes, "explain")
	}
	if m.Keepdepfile {
		modes = append(modes, "keepdepfile")
	}
	if m.Keeprsp {
		modes = append(modes, "keeprsp")
	}
	return strings.Join(modes, ",")
}

// Set sets flag value for each flag present.
func (m *debugMode) Set(v string) error {
	for s := range strings.SplitSeq(v, ",") {
		switch s {
		case "stats":
			m.Stats = true
		case "explain":
			m.Explain = true
		case "keepdepfile":
			m.Keepdepfile = true
		case "keeprsp":
			m.Keeprsp = true
		case "list":
			m.List = true
		default:
			return fmt.Errorf("unknown debug setting %q", s)
		}
	}
	return nil
}

type explainDebugWriter struct {
	w io.Writer
}

func (w explainDebugWriter) Write(buf []byte) (int, error) {
	_, err := fmt.Fprintf(w.w, "ninja explain: %s", buf)
	return len(buf), err
}

func newExplainWriter(w io.Writer, fname string) io.Writer {
	if fname != "" {
		fmt.Fprintf(w, "ninja explain: recorded in %s even without `-d explain`\n", fname)
	}
	return explainDebugWriter{w: w}
}
