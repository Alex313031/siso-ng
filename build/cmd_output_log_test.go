// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"errors"
	"testing"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/ui"
)

func TestCmdOutput(t *testing.T) {
	execcmd := &execute.Cmd{
		Desc:       "CXX foo.o",
		ActionName: "cxx",
		Dir:        "out/siso",
		Outputs:    []string{"out/siso/foo.o"},
	}
	const command = "../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc"

	for _, tc := range []struct {
		name           string
		result         cmdOutputResult
		stdout, stderr []byte
		rule           string
		err            error
		want           string
	}{
		{
			name:   "successNoOutErr",
			result: cmdOutputResultSUCCESS,
		},
		{
			name:   "successOut",
			result: cmdOutputResultSUCCESS,
			stdout: []byte("warning: warning message\n"),
			rule:   "clang/cxx",
			want: `SUCCESS:  "./foo.o" CXX foo.o
build step: cxx "./foo.o"
siso_rule: clang/cxx
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc
stdout:
warning: warning message
`,
		},
		{
			name:   "successOutNoRule",
			result: cmdOutputResultSUCCESS,
			stdout: []byte("warning: warning message\n"),
			want: `SUCCESS:  "./foo.o" CXX foo.o
build step: cxx "./foo.o"
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc
stdout:
warning: warning message
`,
		},
		{
			name:   "successErr",
			result: cmdOutputResultSUCCESS,
			stderr: []byte("error: error message\n"),
			rule:   "clang/cxx",
			want: `SUCCESS:  "./foo.o" CXX foo.o
build step: cxx "./foo.o"
siso_rule: clang/cxx
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc
stderr:
error: error message
`,
		},
		{
			name:   "successErrNoEOL",
			result: cmdOutputResultSUCCESS,
			stderr: []byte("error: error message"),
			rule:   "clang/cxx",
			want: `SUCCESS:  "./foo.o" CXX foo.o
build step: cxx "./foo.o"
siso_rule: clang/cxx
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc
stderr:
error: error message
`,
		},
		{
			name:   "error",
			result: cmdOutputResultFAILED,
			rule:   "clang/cxx",
			err:    errors.New("failed to exec: exit=1"),
			want: `FAILED:  "./foo.o" CXX foo.o
err: failed to exec: exit=1
build step: cxx "./foo.o"
siso_rule: clang/cxx
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc
`,
		},
		{
			name:   "errorNoRule",
			result: cmdOutputResultFAILED,
			err:    errors.New("failed to exec: exit=1"),
			want: `FAILED:  "./foo.o" CXX foo.o
err: failed to exec: exit=1
build step: cxx "./foo.o"
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			cmd := &execute.Cmd{}
			*cmd = *execcmd
			if len(tc.stdout) > 0 {
				w := cmd.StdoutWriter()
				w.Write(tc.stdout)
			}
			if len(tc.stderr) > 0 {
				w := cmd.StderrWriter()
				w.Write(tc.stderr)
			}
			res := cmdOutput(ctx, tc.result, cmd, command, tc.rule, tc.err)
			if got := res.String(); got != tc.want {
				t.Errorf("cmdOutput got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestCmdOutputMsg(t *testing.T) {
	execcmd := &execute.Cmd{
		Desc:       "CXX foo.o",
		ActionName: "cxx",
		Dir:        "out/siso",
		Outputs:    []string{"out/siso/foo.o"},
	}
	const shortCommand = "python3 ../../build/gen.py gen/base.txt"
	const longCommand = "../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc -o obj/base/base.o"

	for _, tc := range []struct {
		name            string
		command         string
		width           int
		verboseFailures bool
		want            string
	}{
		{
			name:    "short",
			command: shortCommand,
			want: `FAILED:  "./foo.o" CXX foo.o
err: exit=1
python3 ../../build/gen.py gen/base.txt
build step: cxx "./foo.o"
`,
		},
		{
			name:    "shortWide",
			command: shortCommand,
			width:   100,
			want: `FAILED:  "./foo.o" CXX foo.o
err: exit=1
python3 ../../build/gen.py gen/base.txt
build step: cxx "./foo.o"
`,
		},
		{
			name:            "shortVerbose",
			command:         shortCommand,
			width:           100,
			verboseFailures: true,
			want: `FAILED:  "./foo.o" CXX foo.o
err: exit=1
python3 ../../build/gen.py gen/base.txt
build step: cxx "./foo.o"
`,
		},
		{
			name:    "long",
			command: longCommand,
			want: `FAILED:  "./foo.o" CXX foo.o
err: exit=1
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc -o obj/base/base.o
build step: cxx "./foo.o"
`,
		},
		{
			name:    "longNarrow",
			command: longCommand,
			width:   80,
			want: `FAILED:  "./foo.o" CXX foo.o
err: exit=1
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../.  ...(too long)
Use '--verbose_failures' to see the command lines
build step: cxx "./foo.o"
`,
		},
		{
			name:            "longNarrowVerbose",
			command:         longCommand,
			width:           80,
			verboseFailures: true,
			want: `FAILED:  "./foo.o" CXX foo.o
err: exit=1
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc -o obj/base/base.o
build step: cxx "./foo.o"
`,
		},
		{
			name:    "longWide",
			command: longCommand,
			width:   160,
			want: `FAILED:  "./foo.o" CXX foo.o
err: exit=1
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc -o obj/base/base.o
build step: cxx "./foo.o"
`,
		},
		{
			name:            "longWideVerbose",
			command:         longCommand,
			width:           160,
			verboseFailures: true,
			want: `FAILED:  "./foo.o" CXX foo.o
err: exit=1
../../third_party/llvm-build/Release+Asserts/bin/clang++ -c ../../base/base.cc -o obj/base/base.o
build step: cxx "./foo.o"
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			cmd := &execute.Cmd{}
			*cmd = *execcmd
			res := cmdOutput(ctx, cmdOutputResultFAILED, cmd, tc.command, "", errors.New("exit=1"))
			if res == nil {
				t.Fatalf("res=nil; want non-nil")
			}
			if got := ui.StripANSIEscapeCodes(res.Msg(tc.width, false, tc.verboseFailures)); got != tc.want {
				t.Errorf("cmdOutput.Msg(%d,concole=false,verboseFailures=%t) got:\n%s\nwant:\n%s", tc.width, tc.verboseFailures, got, tc.want)
			}
		})
	}
}
