// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package localexec implements local command execution.
package localexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
	epb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/sync/semaphore"
	"go.chromium.org/build/siso/ui"
)

// TODO(b/270886586): Compare local execution with/without local execution server.

// WorkerName is a name used for worker of the cmd in action result.
const WorkerName = "local"

// LocalExec implements execute.Executor interface that runs commands locally.
type LocalExec struct{}

// Run runs cmd with DefaultExec.
func Run(ctx context.Context, cmd *execute.Cmd) error {
	return LocalExec{}.Run(ctx, cmd)
}

// Run runs a cmd.
func (LocalExec) Run(ctx context.Context, cmd *execute.Cmd) (err error) {
	var res *rpb.ActionResult
	if cmd.Console {
		// Console actions need real stdin and output teed to the terminal, so
		// they always run in-process rather than through the spawn helper.
		res, err = run(ctx, cmd)
	} else {
		// Otherwise prefer the out-of-process spawn helper so the large-heap
		// siso process never fork()s (a no-op direct exec on non-unix).
		res, err = runViaHelper(ctx, cmd)
	}
	if err != nil {
		return err
	}
	cmd.SetActionResult(res, false)

	duration := res.ExecutionMetadata.ExecutionCompletedTimestamp.AsTime().Sub(res.ExecutionMetadata.ExecutionStartTimestamp.AsTime())
	clog.Infof(ctx, "localexec: %v duration=%s exit=%d stdout=%d stderr=%d metadata=%s", cmd.Args, duration, res.ExitCode, len(res.StdoutRaw), len(res.StderrRaw), res.ExecutionMetadata)

	if res.ExitCode != 0 {
		return execute.ExitError{ExitCode: int(res.ExitCode)}
	}
	if cmd.HashFS == nil {
		return nil
	}
	// TODO(b/254158307): calculate action digest if cmd is pure?
	now := time.Now()
	return cmd.RecordOutputsFromLocal(ctx, now)
}

// fix for http://b/278658064 windows: fork/exec: Not enough memory resources are available to process this command.
var ForkSema = semaphore.New("fork", runtime.GOMAXPROCS(0))

// run runs cmd, retrying with bounded backoff when starting the process
// fails with ETXTBSY, i.e. a write-mode fd to the executable is transiently
// open somewhere (https://github.com/golang/go/issues/22315).
func run(ctx context.Context, cmd *execute.Cmd) (*rpb.ActionResult, error) {
	backoff := 10 * time.Millisecond
	for {
		res, err := runOnce(ctx, cmd)
		if err == nil || !errors.Is(err, syscall.ETXTBSY) || backoff > 320*time.Millisecond {
			// success, non-ETXTBSY error, or budget exhausted.
			return res, err
		}
		clog.Warningf(ctx, "ETXTBSY starting %s; retrying in %s", cmd.Args[0], backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return res, context.Cause(ctx)
		}
		backoff *= 2
	}
}

func runOnce(ctx context.Context, cmd *execute.Cmd) (*rpb.ActionResult, error) {
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("no arguments in the command. ID: %s", cmd.ID)
	}
	c := exec.CommandContext(ctx, cmd.Args[0], cmd.Args[1:]...)
	if cmd.Console {
		// Console actions stay in siso's foreground group (else terminal reads
		// stop them with SIGTTIN), so cancel sends SIGINT to just the direct
		// child rather than the process group.
		c.Cancel = func() error {
			err := c.Process.Signal(os.Interrupt)
			clog.Warningf(ctx, "send interrupt to pid=%d: %v", c.Process.Pid, err)
			return err
		}
	} else {
		// Run the action as the leader of its own process group, so that
		// cancellation and post-exit reaping reach every descendant, not just
		// the direct child (no-ops on Windows).
		setProcGroup(c)
		c.Cancel = func() error {
			return cancelGroup(ctx, c)
		}
	}
	// Completion contract, same as Ninja's: the step is done when the direct
	// child has exited AND its stdout/stderr pipes hit EOF, i.e. when every
	// descendant holding the inherited fds has finished - a descendant the
	// action didn't wait for may still be writing outputs. WaitDelay stays zero
	// on purpose: bounding the wait can only fail a slow straggler (the v1.5.17
	// Windows CI regression) or kill it and record truncated outputs as
	// success. (Bazel kills survivors at main-child exit instead, which is only
	// safe because it captures output via files, not pipes.)
	c.Env = cmd.Env
	c.Dir = filepath.Join(cmd.WorkspaceRoot, cmd.WorkDir)
	c.Stdout = cmd.StdoutWriter()
	c.Stderr = cmd.StderrWriter()
	var consoleWG sync.WaitGroup
	var consoleCancel func()
	if cmd.Console {
		c.Stdin = os.Stdin

		// newline after siso status line if cmd outputs.
		checkClose := func(f *os.File, s string) {
			err := f.Close()
			if err != nil && !errors.Is(err, fs.ErrClosed) {
				clog.Warningf(ctx, "close %s: %v", s, err)
			}
		}
		stdoutr, stdoutw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer checkClose(stdoutr, "stdout(r)")
		defer checkClose(stdoutw, "stdout(w)")
		stderrr, stderrw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer checkClose(stderrr, "stderr(r)")
		defer checkClose(stderrw, "stderr(w)")

		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		consoleCancel = func() {
			cancel()
			checkClose(stdoutr, "stdout(r)")
			checkClose(stdoutw, "stdout(w)")
			checkClose(stderrr, "stderr(r)")
			checkClose(stderrw, "stderr(w)")
		}
		consoleOut := func(r io.Reader, w io.Writer, s string) {
			defer consoleWG.Done()
			var buf [1]byte
			n, err := r.Read(buf[:])
			if err != nil {
				clog.Warningf(rctx, "consoleOut %s: read %v", s, err)
				return
			}
			if !cmd.ConsoleOut.Swap(true) {
				fmt.Fprintln(w)
			}
			_, err = w.Write(buf[:n])
			if err != nil {
				clog.Warningf(rctx, "console write %s: %v", s, err)
			}
			_, err = io.Copy(w, r)
			if err != nil && !errors.Is(err, os.ErrClosed) {
				clog.Warningf(rctx, "console copy %s: %v", s, err)
			}
		}

		consoleWG.Add(2)
		go consoleOut(stdoutr, os.Stdout, "stdout")
		go consoleOut(stderrr, os.Stderr, "stderr")

		c.Stdout = io.MultiWriter(stdoutw, c.Stdout)
		c.Stderr = io.MultiWriter(stderrw, c.Stderr)
	}
	s := time.Now()

	var ru *epb.Rusage
	var err error
	var tracker *rusageTracker
	err = ForkSema.Do(ctx, func(ctx context.Context) error {
		return c.Start()
	})
	if err == nil {
		tracker = newRusageTracker(c)
		if cmd.OOMScoreAdj != nil {
			oomScoreAdj(ctx, c.Process.Pid, *cmd.OOMScoreAdj)
		}
		err = c.Wait()
		if !cmd.Console {
			// Sweep the action's process group (pgid == pid, see setProcGroup).
			// Anything alive in-group at EOF already closed its pipe fds, so it
			// is a half-detached leftover, not an output writer; setsid daemons
			// intentionally escape. On cancellation this is the authoritative
			// reap after cancelGroup's group kill.
			drainGroup(ctx, c.Process.Pid)
		}
	}
	if tracker != nil {
		ru = tracker.Rusage()
		tracker.Close()
	}
	if cmd.Console {
		consoleCancel()
		consoleWG.Wait()
		if ui.IsTerminal() && cmd.ConsoleOut.Load() {
			ui.Default.Printf("\n\n\n\n") // preserve console output from progress report
		}
	}
	e := time.Now()

	code := exitCode(err)

	result := &rpb.ActionResult{
		ExitCode:  code,
		StdoutRaw: cmd.Stdout(),
		StderrRaw: cmd.Stderr(),
		ExecutionMetadata: &rpb.ExecutedActionMetadata{
			Worker:                      WorkerName,
			ExecutionStartTimestamp:     timestamppb.New(s),
			ExecutionCompletedTimestamp: timestamppb.New(e),
		},
	}
	if ru != nil {
		p, err := anypb.New(ru)
		if err != nil {
			clog.Warningf(ctx, "pack rusage: %v", err)
		} else {
			result.ExecutionMetadata.AuxiliaryMetadata = append(result.ExecutionMetadata.AuxiliaryMetadata, p)
		}
	}

	// TODO(b/273423470): track resource usage.

	if code >= 0 {
		err = nil
	}
	return result, err
}

func exitCode(err error) int32 {
	if err == nil {
		return 0
	}
	eerr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return -1
	}
	return int32(eerr.ExitCode())
}
