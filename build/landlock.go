// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
package build

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/subcommands"
	"github.com/landlock-lsm/go-landlock/landlock"
	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
)

type landlockExecutor struct {
	innerExecutor       execute.Executor
	tool                string
	defaultReadableDirs []string
	defaultWritableDirs []string
}

var _ execute.Executor = (*landlockExecutor)(nil)

func newLandlockExecutor(executor execute.Executor, sandboxConfig map[string]string) *landlockExecutor {
	var defaultReadableDirs []string
	var defaultWritableDirs []string
	_, hasReadable := sandboxConfig["default_readable_dirs"]
	_, hasWritable := sandboxConfig["default_writable_dirs"]
	if hasReadable || hasWritable {
		defaultReadableDirs = splitDirs(sandboxConfig["default_readable_dirs"])
		defaultWritableDirs = splitDirs(sandboxConfig["default_writable_dirs"])
	} else {
		// /dev/null and some /proc files need to be writable
		defaultWritableDirs = []string{
			"/dev",
			"/proc",
		}
		defaultReadableDirs = []string{
			"/bin",
			"/lib",
			"/lib64",
			"/usr/bin",
			"/usr/lib",
			"/usr/lib32",
			"/usr/lib64",
		}
	}
	return &landlockExecutor{
		innerExecutor: executor,
		// Users can provide a custom tool to do the landlocking.
		// If not provided, siso will re-exec itself as the tool.
		// Using a custom tool can be faster due to siso's large size.
		tool:                sandboxConfig["landlock_tool_path"],
		defaultReadableDirs: defaultReadableDirs,
		defaultWritableDirs: defaultWritableDirs,
	}
}

func splitDirs(s string) []string {
	dirs := []string{}
	for _, d := range filepath.SplitList(s) {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func (l *landlockExecutor) Run(ctx context.Context, cmd *execute.Cmd) error {
	workspaceRoot := filepath.Clean(cmd.WorkspaceRoot)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// We allow full read/write access to any directory that contains an output
	// file. This is because it's a common pattern to create a $out.tmp file
	// in the same directory and read/write to it. Unfortunately this means that
	// some unrelated files in the same directory can be read/written by the
	// action, but this is the best we can do with landlock AFAICT.
	// We also can't read these directories and add denylist rules for all the
	// existing files in them because landlock starts with 0 permissions and
	// only supports adding permissions. Once they've been added they can't
	// be taken away again.
	allOutputs := cmd.AllOutputs()

	outputDirsWithSlash := make([]string, 0, len(allOutputs))
	for _, out := range allOutputs {
		d := filepath.Dir(out.String())
		if isAncestor(cwd, d, workspaceRoot) {
			return fmt.Errorf("Output files cannot be directly under the workspace root, or "+
				"else the whole root would be made writable, defeating the point of action "+
				"sandboxing: %s", out.String())
		}
		outputDirsWithSlash = append(outputDirsWithSlash, d+"/")
	}
	slices.Sort(outputDirsWithSlash)

	var outputDirs []string
	var lastAncestor string
	for _, d := range outputDirsWithSlash {
		if lastAncestor != "" && strings.HasPrefix(d, lastAncestor) {
			continue
		}
		lastAncestor = d
		if !filepath.IsAbs(d) {
			d = filepath.Join(workspaceRoot, d)
		} else {
			d = d[:len(d)-1]
		}
		outputDirs = append(outputDirs, d)
	}

	tmpdir := os.Getenv("TMPDIR")
	if tmpdir != "" {
		tmpdir = filepath.Clean(tmpdir)
		if !slices.ContainsFunc(outputDirs, func(d string) bool { return isAncestor(cwd, d, tmpdir) }) &&
			!isAncestor(cwd, tmpdir, workspaceRoot) {
			outputDirs = append(outputDirs, tmpdir)
		}
	}
	// Even if TMPDIR is set, some actions don't respect it and use /tmp instead.
	if tmpdir != "/tmp" && !isAncestor(cwd, "/tmp", workspaceRoot) {
		outputDirs = append(outputDirs, "/tmp")
	}

	for _, d := range l.defaultWritableDirs {
		if !filepath.IsAbs(d) && workspaceRoot != "" {
			d = filepath.Join(workspaceRoot, d)
		}
		outputDirs = append(outputDirs, d)
	}

	allInputs := cmd.AllInputs()
	roDirs := make([]string, 0, len(l.defaultReadableDirs)+len(allInputs))
	for _, d := range l.defaultReadableDirs {
		if !filepath.IsAbs(d) && workspaceRoot != "" {
			d = filepath.Join(workspaceRoot, d)
		}
		roDirs = append(roDirs, d)
	}
	roFiles := make([]string, 0, len(allInputs))
	for _, in := range allInputs {
		p := in.String()
		if !filepath.IsAbs(p) && workspaceRoot != "" {
			p = filepath.Join(workspaceRoot, p)
		}
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			// This shouldn't happen, but if it does, just
			// continue running the command and let it fail.
		} else if err != nil {
			return err
		} else if info.Mode()&fs.ModeSymlink != 0 {
			// Landlock never restricts access to readlink.
			// Don't add it as an roFile because landlock
			// will read through the link and give access to
			// the underlying file.
			// It would also cause a failure if we added
			// a dangling symlink to roFiles.
			// This requires rules to add dependencies on
			// both the symlink themselves and the targets
			// of the symlinks to be able to read the symlinks
			// properly, that's intentional.
			// The nsjail sandboxing works the same way.
		} else if info.Mode()&fs.ModeDir != 0 {
			roDirs = append(roDirs, p)
		} else {
			roFiles = append(roFiles, p)
		}
	}

	req := landlockRequest{
		// Some tools, like rustc, expect to be able to list a directory to find
		// input files. We allow reading all directories in the system to allow
		// this behavior, as we don't know how many parent directories up it's
		// going to start reading from.
		AllowGlobalReaddir: true,
		RODirs:             roDirs,
		ROFiles:            roFiles,
		RWDirs:             outputDirs,
		Args:               cmd.Args,
	}

	data, err := json.MarshalIndent(&req, "", "  ")
	if err != nil {
		return err
	}

	f, err := os.CreateTemp("", "landlock_config_*.json")
	if err != nil {
		return err
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	f.Close()

	path := f.Name()

	var args []string
	if l.tool == "" {
		sisoBin, err := os.Executable()
		if err != nil {
			return err
		}
		args = []string{sisoBin, "landlock", path}
	} else {
		args = []string{l.tool, path}
	}

	cmd.StdoutWriter()
	cmd.StderrWriter()
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	newCmd.Args = args
	clog.Infof(ctx, "run %q", newCmd.Args)
	err = l.innerExecutor.Run(ctx, newCmd)
	cmd.SetActionResult(newCmd.ActionResult())
	if err != nil {
		return fmt.Errorf("failed to run landlocked command with config %s: %w", path, err)
	}
	if err := os.Remove(f.Name()); err != nil {
		clog.Warningf(ctx, "Failed to remove %s: %s", f.Name(), err)
	}
	return nil
}

type landlockRequest struct {
	AllowGlobalReaddir bool
	RODirs             []string
	RWDirs             []string
	ROFiles            []string
	Args               []string
}

// Cmd returns the Command for the `landlock` subcommand,
// which accepts a json file of type landlockRequest and runs
// a subcommand under the given landlock restrictions.
func LandlockCmd() *LandLockCommand {
	return &LandLockCommand{}
}

type LandLockCommand struct{}

var _ subcommands.Command = (*LandLockCommand)(nil)

func (l *LandLockCommand) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if len(f.Args()) != 1 {
		fmt.Fprintf(os.Stderr, "Expected exactly 1 argument, got %d\n", len(f.Args()))
		return subcommands.ExitFailure
	}
	data, err := os.ReadFile(f.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %s\n", f.Arg(0), err)
		return subcommands.ExitFailure
	}
	var req landlockRequest
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to unmarshal %s: %s\n", f.Arg(0), err)
		return subcommands.ExitFailure
	}

	rules := make([]landlock.Rule, 0, 4)
	if req.AllowGlobalReaddir {
		rules = append(rules, landlock.PathAccess(ll.AccessFSReadDir, "/"))
	}
	rules = append(rules, landlock.RODirs(req.RODirs...))
	rules = append(rules, landlock.ROFiles(req.ROFiles...))
	rules = append(rules, landlock.RWDirs(req.RWDirs...).WithRefer())

	if err := landlock.V5.RestrictPaths(rules...); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to enable landlock: %s\n", err)
		return subcommands.ExitFailure
	}

	c := exec.Command(req.Args[0], req.Args[1:]...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
			return subcommands.ExitStatus(exitError.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "Failed to execute command %s: %s\n", strings.Join(req.Args, " "), err)
		return subcommands.ExitFailure
	}

	return subcommands.ExitSuccess
}

func (l *LandLockCommand) Name() string {
	return "landlock"
}

func (l *LandLockCommand) SetFlags(*flag.FlagSet) {
	// no flags, just 1 positional argument
}

func (l *LandLockCommand) Synopsis() string {
	return "runs a command with landlock"
}

func (l *LandLockCommand) Usage() string {
	return "landlock <config json>\n"
}

// IsAncestor returns true if p1 is an ancestor of p2.
// Identical directories are considered ancestors.
// Requires that both p1 and p2 are clean paths (filepath.Clean()).
func isAncestor(cwd, p1, p2 string) bool {
	// It's very difficult to get the correct behavior with relative
	// paths or mixtures of relative and absolute paths, make them absolute.
	if !filepath.IsAbs(p1) {
		p1 = filepath.Join(cwd, p1)
	}
	if !filepath.IsAbs(p2) {
		p2 = filepath.Join(cwd, p2)
	}
	rel, err := filepath.Rel(p1, p2)
	if err != nil {
		return false
	}
	return filepath.IsLocal(rel)
}
