// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/metadata"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/toolsupport/reclientutil"
)

// logWriters holds writers for various logs that are written to over the
// course of the ninja build.
//
// Note that there are also other one-off logs that are not held here.
type logWriters struct {
	failureSummaryWriter io.Writer
	failedCommandsWriter io.Writer
	outputLogWriter      io.Writer
	explainWriter        io.Writer
	localexecLogWriter   io.Writer
	metricsJSONWriter    io.Writer
}

// File name of siso result file.
const sisoResultFilename = "siso_result.json"

// SisoResult contains siso result information.
type SisoResult struct {
	Code         int    `json:"code,omitempty"`
	InfraFailure bool   `json:"infra_failure,omitempty"`
	Message      string `json:"message,omitempty"`
}

func (c *Command) rotateSisoResult(ctx context.Context) {
	rotateFiles(ctx, filepath.Join(c.logDir, sisoResultFilename))
}

func (c *Command) writeSisoResult(result SisoResult) error {
	buf, err := json.MarshalIndent(result, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.logDir, sisoResultFilename), buf, 0644)
}

type failureSummaryWriter struct {
	mu       sync.Mutex
	filename string
}

func (w *failureSummaryWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := f.Write(data)
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	return n, err
}

type cleanupFunc func(*error)

func (c *Command) initLogWriters(ctx context.Context, buildPath *build.Path) (logWriters, cleanupFunc, error) {
	var writers logWriters
	var dones []cleanupFunc
	var err error
	var done cleanupFunc

	if fname := c.logFilename(c.failureSummaryFile, ""); fname != "" {
		writers.failureSummaryWriter = &failureSummaryWriter{
			filename: fname,
		}
	}
	dones = append(dones, func(errp *error) {
		if writers.failureSummaryWriter != nil && *errp != nil {
			fmt.Fprintf(writers.failureSummaryWriter, "error: %v\n", *errp)
		}
	})

	writers.failedCommandsWriter, done, err = c.logWriter(ctx, c.failedCommandsFile)
	if err != nil {
		return writers, nil, err
	}
	dones = append(dones, done)
	newline := "\n"
	if runtime.GOOS != "windows" {
		if f, ok := writers.failedCommandsWriter.(*os.File); ok {
			err = f.Chmod(0755)
			if err != nil {
				return writers, nil, err
			}
		}
		fmt.Fprintf(writers.failedCommandsWriter, "#!/bin/sh\n")
		fmt.Fprintf(writers.failedCommandsWriter, "set -ve\n")
	} else {
		newline = "\r\n"
	}
	fmt.Fprintf(writers.failedCommandsWriter, "cd %s%s", buildPath.AbsBase(), newline)
	// TODO: for reproxy mode, may need to run reproxy for rewrapper commands.

	writers.outputLogWriter, done, err = c.logWriter(ctx, c.outputLogFile)
	if err != nil {
		return writers, nil, err
	}
	dones = append(dones, done)

	writers.explainWriter, done, err = c.logWriter(ctx, c.explainFile)
	if err != nil {
		return writers, nil, err
	}
	dones = append(dones, done)
	if c.debugMode.Explain {
		if writers.explainWriter == nil {
			writers.explainWriter = newExplainWriter(os.Stderr, "")
		} else {
			writers.explainWriter = io.MultiWriter(newExplainWriter(os.Stderr, c.logFilename(c.explainFile, c.startDir)), writers.explainWriter)
		}
	}

	writers.localexecLogWriter, done, err = c.logWriter(ctx, c.localexecLogFile)
	if err != nil {
		return writers, nil, err
	}
	dones = append(dones, done)

	writers.metricsJSONWriter, done, err = c.logWriter(ctx, c.metricsJSON)
	if err != nil {
		return writers, nil, err
	}
	dones = append(dones, done)

	return writers, func(err *error) {
		for i := len(dones) - 1; i >= 0; i-- {
			dones[i](err)
		}
	}, nil
}

func (c *Command) initLogDir(ctx context.Context) error {
	if !filepath.IsAbs(c.logDir) {
		logDir, err := filepath.Abs(c.logDir)
		if err != nil {
			return fmt.Errorf("abspath for log dir: %w", err)
		}
		c.logDir = logDir
	}
	err := os.MkdirAll(c.logDir, 0755)
	if err != nil {
		return err
	}
	return c.logSymlink(ctx)
}

// glogFilename returns filename of glog logfile. i.e. siso.INFO.
func (c *Command) glogFilename() string {
	logFilename := "siso.INFO"
	if runtime.GOOS == "windows" {
		logFilename = "siso.exe.INFO"
	}
	return filepath.Join(c.logDir, logFilename)
}

func rotateFiles(ctx context.Context, fname string) {
	ext := filepath.Ext(fname)
	fnameBase := strings.TrimSuffix(fname, ext)

	oldestFilename := fmt.Sprintf("%s.9%s", fnameBase, ext)
	fi, err := os.Lstat(oldestFilename)
	if err == nil && fi.Mode().Type() == fs.ModeSymlink {
		target, err := os.Readlink(oldestFilename)
		if err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(oldestFilename), target)
			}
			err = os.Remove(target)
			clog.Infof(ctx, "remove oldest log %s: %v", target, err)
		}
	}
	// oldestFilename itself will be replaced with <base>.8<ext>.
	for i := 8; i >= 0; i-- {
		err := os.Rename(
			fmt.Sprintf("%s.%d%s", fnameBase, i, ext),
			fmt.Sprintf("%s.%d%s", fnameBase, i+1, ext))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "rotate %s %d->%d failed: %v", fname, i, i+1, err)
		}
	}
	err = os.Rename(fname, fmt.Sprintf("%s.0%s", fnameBase, ext))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		clog.Warningf(ctx, "rotate %s ->0 failed: %v", fname, err)
	}
}

func (c *Command) logSymlink(ctx context.Context) error {
	logFilename := c.glogFilename()
	rotateFiles(ctx, logFilename)
	logfiles, err := log.Names("INFO")
	if err != nil {
		return fmt.Errorf("failed to get glog INFO level log files: %w", err)
	}
	if len(logfiles) == 0 {
		return fmt.Errorf("no glog INFO level log files")
	}
	err = os.Symlink(logfiles[0], logFilename)
	if err != nil {
		clog.Warningf(ctx, "failed to create %s: %v", logFilename, err)
		// On Windows, it failed to create symlink.
		// just same filename in *.redirected file.
		err = os.WriteFile(logFilename+".redirected", []byte(logfiles[0]), 0644)
		if err != nil {
			clog.Warningf(ctx, "failed to write %s.redirected: %v", logFilename, err)
		}
		c.sisoInfoLog = logfiles[0]
		return nil
	}
	clog.Infof(ctx, "logfile: %q", logfiles)
	c.sisoInfoLog = filepath.Base(logFilename)
	return nil
}

// logFilename returns siso's log filename relative to startDir, or absolute path.
func (c *Command) logFilename(fname, startDir string) string {
	if fname == "" {
		return ""
	}
	if !filepath.IsAbs(fname) {
		fname = filepath.Join(c.logDir, fname)
	}
	if startDir == "" {
		return fname
	}
	rel, err := filepath.Rel(startDir, fname)
	if err != nil || !filepath.IsLocal(rel) {
		return fname
	}
	return "." + string(os.PathSeparator) + rel
}

func (c *Command) logWriter(ctx context.Context, fname string) (io.Writer, func(errp *error), error) {
	fname = c.logFilename(fname, "")
	if fname == "" {
		return nil, func(*error) {}, nil
	}
	rotateFiles(ctx, fname)
	f, err := os.Create(fname)
	if err != nil {
		return nil, func(*error) {}, err
	}
	return f, func(errp *error) {
		clog.Infof(ctx, "close %s", fname)
		cerr := f.Close()
		if *errp == nil {
			*errp = cerr
		}
	}, nil
}

func (c *Command) setupCrashOutput(ctx context.Context) (func(), error) {
	fname := c.logFilename("siso_crash", "")
	rotateFiles(ctx, fname)
	crashFile, err := os.Create(fname)
	if err != nil {
		return nil, err
	}
	err = debug.SetCrashOutput(crashFile, debug.CrashOptions{})
	if err != nil {
		return nil, err
	}
	return func() { debug.SetCrashOutput(nil, debug.CrashOptions{}) }, crashFile.Close()
}

func (c *Command) writeInvocationInfo(ctx context.Context, metricsLabels map[string]string, targets []string) error {
	j, err := json.Marshal(metadata.InvocationInfo{
		SisoVersion:   c.version,
		StartTime:     c.started,
		BuildID:       c.buildID,
		Targets:       targets,
		MetricsLabels: metricsLabels,
		Machine:       metadata.GatherMachineInfo(ctx),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.logDir, c.invocationJSON), j, 0644)
}

func (c *Command) cleanupReclientMetrics(ctx context.Context) {
	for _, f := range []string{"rbe_metrics.pb", "rbe_metrics.txt"} {
		file := path.Join(c.logDir, f)
		err := os.Remove(file)
		if err == nil {
			clog.Infof(ctx, "removed %q", file)
		} else {
			if errors.Is(err, os.ErrNotExist) {
				clog.Infof(ctx, "%q does not exit", file)
			} else {
				clog.Warningf(ctx, "failed to remove %q. %v", f, err)
			}
		}
	}
}

func (c *Command) writeReclientMetrics(dur time.Duration, stats build.Stats) error {
	m := reclientutil.RBEBuildMetrics(c.buildID, c.version, dur, stats)
	mb, err := proto.Marshal(m)
	if err != nil {
		return fmt.Errorf("failed to marshal RBE build metrics. %w", err)
	}
	if err := os.WriteFile(path.Join(c.logDir, "rbe_metrics.pb"), mb, 0644); err != nil {
		return fmt.Errorf("failed to write iRBE build metrics. %w", err)
	}
	opts := prototext.MarshalOptions{
		Multiline: true,
		Indent:    "  ",
	}
	mt, err := opts.Marshal(m)
	if err != nil {
		return fmt.Errorf("failed to marshal RBE build metrics. %w", err)
	}
	if err := os.WriteFile(path.Join(c.logDir, "rbe_metrics.txt"), mt, 0644); err != nil {
		return fmt.Errorf("failed to write iRBE build metrics. %w", err)
	}
	return nil
}
