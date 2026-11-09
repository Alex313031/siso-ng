// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ps

import (
	"strings"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/ui"
)

type mockUI struct {
	lines []string
}

func (m *mockUI) PrintLines(lines ...string) {
	m.lines = append(m.lines, lines...)
}

func (m *mockUI) IsOneTerminalLine(string) bool { return true }
func (m *mockUI) NewSpinner() ui.Spinner        { return nil }
func (m *mockUI) Printf(string, ...any)         {}
func (m *mockUI) Infof(string, ...any)          {}
func (m *mockUI) Warningf(string, ...any)       {}
func (m *mockUI) Errorf(string, ...any)         {}

func TestFormatHeader(t *testing.T) {
	for _, tc := range []struct {
		name     string
		loc      string
		progress build.ProgressInfo
		want     string
	}{
		{
			name: "no_progress_total",
			loc:  "/path/to/out",
			progress: build.ProgressInfo{
				Done:  0,
				Total: 0,
			},
			want: "Siso is running in /path/to/out",
		},
		{
			name: "with_progress",
			loc:  "/path/to/out",
			progress: build.ProgressInfo{
				Done:  120,
				Total: 500,
			},
			want: "Siso is running in /path/to/out [120/500 (remaining: 380)]",
		},
		{
			name: "done_exceeds_total",
			loc:  "/path/to/out",
			progress: build.ProgressInfo{
				Done:  550,
				Total: 500,
			},
			want: "Siso is running in /path/to/out [550/500 (remaining: 0)]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatHeader(tc.loc, tc.progress)
			if got != tc.want {
				t.Errorf("formatHeader(%q, %+v) = %q, want %q", tc.loc, tc.progress, got, tc.want)
			}
		})
	}
}

func TestCommand_RenderProgress(t *testing.T) {
	oldUI := ui.Default
	mUI := &mockUI{}
	ui.Default = mUI
	defer func() { ui.Default = oldUI }()

	cmd := &Command{
		loc: "/path/to/out",
	}

	progress := build.ProgressInfo{
		Done:  120,
		Total: 500,
		ActiveSteps: []build.ActiveStepInfo{
			{Desc: "compile foo.cc", Phase: "remote", Dur: "3.5s"},
		},
	}

	header := formatHeader(cmd.loc, progress)
	lines := []string{header}
	cmd.render(lines, progress.ActiveSteps)

	if len(mUI.lines) == 0 {
		t.Fatal("no lines rendered")
	}

	combined := strings.Join(mUI.lines, "\n")
	if !strings.Contains(combined, "[120/500 (remaining: 380)]") {
		t.Errorf("rendered output missing progress info: %q", combined)
	}
	if !strings.Contains(combined, "compile foo.cc") {
		t.Errorf("rendered output missing active step: %q", combined)
	}
}
