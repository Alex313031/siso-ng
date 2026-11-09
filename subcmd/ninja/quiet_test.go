// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/ui"
)

func TestQuietUI_BuildActionFinished(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		ui := quietUI{
			Stdout: &stdout,
			Stderr: &stderr,
		}

		cmd := &execute.Cmd{}
		cmd.SetActionResult(&rpb.ActionResult{ExitCode: 0}, false)
		cmd.StdoutWriter().Write([]byte("command stdout\n"))
		cmd.StderrWriter().Write([]byte("command stderr\n"))
		cmd.SetOutputResult("command stdout\ncommand stderr\n")

		step := build.NewStepForTest(1, nil)
		step.SetCmdForTest(cmd)

		ui.BuildActionFinished(step)

		if got, want := stdout.String(), "command stdout\n"; got != want {
			t.Errorf("stdout = %q; want %q", got, want)
		}
		if got, want := stderr.String(), "command stderr\n"; got != want {
			t.Errorf("stderr = %q; want %q", got, want)
		}
	})

	t.Run("failure", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		ui := quietUI{
			Stdout: &stdout,
			Stderr: &stderr,
		}

		cmd := &execute.Cmd{}
		cmd.SetActionResult(&rpb.ActionResult{ExitCode: 1}, false)
		cmd.StdoutWriter().Write([]byte("command stdout\n"))
		cmd.StderrWriter().Write([]byte("error message\n"))
		failedResult := "FAILED: foo.o\n/usr/bin/gcc -c foo.c -o foo.o\ncommand stdout\nerror message\n"
		cmd.SetOutputResult(failedResult)

		step := build.NewStepForTest(1, nil)
		step.SetCmdForTest(cmd)

		ui.BuildActionFinished(step)

		if got, want := stdout.String(), ""; got != want {
			t.Errorf("stdout = %q; want %q", got, want)
		}
		if got, want := stderr.String(), failedResult; got != want {
			t.Errorf("stderr = %q; want %q", got, want)
		}
	})
}

func TestQuietSpinner(t *testing.T) {
	var stdout bytes.Buffer
	ui := quietUI{
		Stdout:          &stdout,
		heartbeatPeriod: 10 * time.Millisecond,
	}

	spinner := ui.NewSpinner()
	spinner.Start("start")
	time.Sleep(35 * time.Millisecond)
	spinner.Stop(nil)

	if !strings.Contains(stdout.String(), ".") {
		t.Errorf("expected heartbeat dots on stdout, got %q", stdout.String())
	}
}

func TestQuietUI_PrintPsHint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		outDir string
		want   string
	}{
		{
			name:   "default_dir",
			outDir: ".",
			want:   "Run `siso ps` in another terminal to see build status.\n",
		},
		{
			name:   "empty_dir",
			outDir: "",
			want:   "Run `siso ps` in another terminal to see build status.\n",
		},
		{
			name:   "custom_dir",
			outDir: "out/Default",
			want:   "Run `siso ps -C out/Default` in another terminal to see build status.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			ui := quietUI{
				Stdout: &stdout,
				Stderr: &stderr,
			}
			ui.printPsHint(tc.outDir)
			if got := stdout.String(); got != "" {
				t.Errorf("stdout = %q; want empty", got)
			}
			if got := stderr.String(); got != tc.want {
				t.Errorf("stderr = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestQuietUI_ChangeToWorkdir(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out", "Default")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "build.ninja"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	origUI := ui.Default
	defer func() { ui.Default = origUI }()

	t.Run("quiet_build", func(t *testing.T) {
		t.Chdir(dir)
		var stdout, stderr bytes.Buffer
		ui.Default = quietUI{
			Stdout: &stdout,
			Stderr: &stderr,
		}
		c := &Command{
			outDir: ninjabuild.DirFlag{Dir: "out/Default"},
			quiet:  true,
			fname:  "build.ninja",
		}
		_, err := c.changeToWorkdir(t.Context())
		if err != nil {
			t.Fatalf("changeToWorkdir failed: %v", err)
		}
		want := "Run `siso ps -C out/Default` in another terminal to see build status.\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q; want %q", got, want)
		}
		if got := stdout.String(); got != "" {
			t.Errorf("stdout = %q; want empty", got)
		}
	})

	t.Run("quiet_subtool", func(t *testing.T) {
		t.Chdir(dir)
		var stdout, stderr bytes.Buffer
		ui.Default = quietUI{
			Stdout: &stdout,
			Stderr: &stderr,
		}
		c := &Command{
			outDir:  ninjabuild.DirFlag{Dir: "out/Default"},
			quiet:   true,
			subtool: "cleandead",
			fname:   "build.ninja",
		}
		_, err := c.changeToWorkdir(t.Context())
		if err != nil {
			t.Fatalf("changeToWorkdir failed: %v", err)
		}
		if got := stderr.String(); got != "" {
			t.Errorf("stderr = %q; want empty for subtool", got)
		}
	})
}
