// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninja implements the subcommand `ninja` which parses a `build.ninja` file and builds the requested targets.
package ninja

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/profiler"
	cloudmetric "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	log "github.com/golang/glog"
	"github.com/google/subcommands"
	"github.com/google/uuid"
	"github.com/klauspost/cpuid/v2"
	"go.opentelemetry.io/otel"
	smetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sync/errgroup"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	rspb "google.golang.org/genproto/googleapis/devtools/resultstore/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/buildconfig"
	"go.chromium.org/build/siso/build/cachestore"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/subcmd/alex313031"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/monitoring"
	"go.chromium.org/build/siso/o11y/resultstore"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/signals"
	"go.chromium.org/build/siso/toolsupport/artfsutil"
	"go.chromium.org/build/siso/toolsupport/cogutil"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
	"go.chromium.org/build/siso/toolsupport/soongutil"
	"go.chromium.org/build/siso/toolsupport/watchmanutil"
	"go.chromium.org/build/siso/ui"
	"go.chromium.org/build/siso/version"
)

// File name of siso metadata file.
// This file is read by ninjalog_uploader.py, in order to populate metadata.
const sisoMetadataFilename = "siso_metadata.json"

// SisoMetadata contains metadata that is populated directly by siso.
type SisoMetadata struct {
	// SisoVersion is the SemVer of siso.
	SisoVersion string `json:"siso_version"`
	// StartTime is the time that the ninja build started.
	StartTime time.Time `json:"start_time"`
	// BuildID is the Ninja build ID used for analytics and identification.
	BuildID string `json:"build_id"`
	// Targets of the build.
	Targets []string `json:"targets,omitempty"`
	// MetricsLabels are arbitrary labels for the build.
	MetricsLabels map[string]string `json:"metrics_labels,omitempty"`
}

const ninjaUsage = `build the requested targets as ninja.

 $ siso ninja [-C <dir>] [options] [targets...]

`

// Cmd returns the Command for the `ninja` subcommand provided by this package.
func Cmd(authOpts cred.Options, version string) *Command {
	return &Command{
		authOpts: authOpts,
		version:  version,
	}
}

// Command implements ninja subcommand.
type Command struct {
	authOpts cred.Options
	version  string
	started  time.Time

	Flags *flag.FlagSet
	// flag values
	dir        string
	configName string
	projectID  string

	buildID string
	jobID   string

	offline         bool
	fastNop         bool
	fastLocal       bool
	fastLastFailure bool // TODO: Always prioritize last failed targets.
	fastExit        bool

	quiet           bool
	verbose         bool
	verboseFailures bool

	dryRun          bool
	clobber         bool
	prepare         bool
	strictRemote    bool
	failuresAllowed int
	actionSalt      string

	ninjaJobs      int
	ninjaLoadLimit int

	remoteJobs int
	localJobs  int
	fname      string

	cacheDir         string
	localCacheEnable bool
	cacheEnableRead  bool
	// cacheEnableWrite bool

	configRepoDir  string
	configFilename string

	outputLocalStrategy string

	depsLogFile string
	// depsLogBucket

	stateDir string

	logDir             string
	frontendFile       string
	failureSummaryFile string
	failedCommandsFile string
	outputLogFile      string
	explainFile        string
	localexecLogFile   string
	metricsJSON        string
	traceJSON          string
	buildPprof         string
	// uploadBuildPprof bool

	fsopt              *hashfs.Option
	reopt              *reapi.Option
	reExecEnable       bool
	reCacheEnableRead  bool
	reCacheEnableWrite bool
	reproxyAddr        string

	artfsDir      string
	artfsEndpoint string

	enableCloudLogging bool
	enableResultstore  bool
	// enableCPUProfiler bool
	enableCloudProfiler         bool
	cloudProfilerServiceName    string
	enableCloudTrace            bool
	enableCloudMonitoring       bool
	enableBuildNinjaFilesUpload bool
	metricsLabels               string
	metricsProject              string
	traceThreshold              time.Duration
	traceSpanThreshold          time.Duration

	subtool    string
	cleandead  bool
	debugMode  debugMode
	adjustWarn string

	sisoInfoLog string // abs or relative to logDir
	startDir    string

	resultstoreUploader *resultstore.Uploader
}

func (*Command) Name() string {
	return "ninja"
}

func (*Command) Synopsis() string {
	return "build the requests targets as ninja"
}

func (*Command) Usage() string {
	return ninjaUsage
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	c.Flags = flagSet
	c.started = time.Now()
	err := parseFlagsFully(flagSet)
	if err != nil {
		ui.Default.Errorf("%v\n", err)
		return subcommands.ExitUsageError
	}
	if c.stateDir == "" {
		c.stateDir = filepath.Dir(c.fname)
	}
	if c.logDir == "" {
		c.logDir = filepath.Dir(c.fname)
	}
	if c.quiet {
		ui.Default = quietUI{}
	} else if c.frontendFile != "" {
		f := os.Stdout
		if c.frontendFile != "-" {
			f, err = os.OpenFile(c.frontendFile, os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				ui.Default.Errorf("failed to open frontend file: %v\n", err)
				return 1
			}
			defer func() {
				err = f.Close()
				if err != nil {
					ui.Default.Errorf("failed to close frontend file: %v\n", err)
				}
			}()
		}
		frontend := soongutil.NewFrontend(ctx, f)
		ui.Default = frontend
		defer frontend.Close()
	}

	stats, err := c.run(ctx)
	d := time.Since(c.started)
	sps := float64(stats.Done-stats.Skipped) / d.Seconds()
	dur := ui.FormatDuration(d)
	if err != nil {
		var errFlag flagError
		var errBuild buildError
		switch {
		case errors.Is(err, errNothingToDo):
			msgPrefix := "Everything is up-to-date"
			if ui.IsTerminal() {
				msgPrefix = ui.SGR(ui.Green, msgPrefix)
			}
			ui.Default.Warningf("%s Nothing to do.\n", msgPrefix)
			return 0

		case errors.As(err, &errFlag):
			ui.Default.Errorf("%v\n", err)

		case errors.As(err, &errBuild):
			var errTarget build.TargetError
			if errors.As(errBuild.err, &errTarget) {
				msgPrefix := "Schedule Failure"
				if ui.IsTerminal() {
					dur = ui.SGR(ui.Bold, dur)
					msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				}
				ui.Default.Errorf("\n%6s %s: %v\n", dur, msgPrefix, errTarget)
				if len(errTarget.Suggests) > 0 {
					var sb strings.Builder
					fmt.Fprintf(&sb, "Did you mean:")
					for _, s := range errTarget.Suggests {
						fmt.Fprintf(&sb, " %q", s)
					}
					fmt.Fprintln(&sb, " ?")
					ui.Default.Warningf("%s\n", sb.String())
				}
				return 1
			}
			var errMissingSource build.MissingSourceError
			if errors.As(errBuild.err, &errMissingSource) {
				msgPrefix := "Schedule Failure"
				if ui.IsTerminal() {
					dur = ui.SGR(ui.Bold, dur)
					msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				}
				ui.Default.Errorf("\n%6s %s: %v\n", dur, msgPrefix, errMissingSource)
				return 1
			}
			msgPrefix := "Build Failure"
			if ui.IsTerminal() {
				dur = ui.SGR(ui.Bold, dur)
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			}
			ui.Default.Errorf("\n%6s %s: %d done, %d failed, %d remaining - %.02f/s\n %v\n", dur, msgPrefix, stats.Done-stats.Skipped, stats.Fail, stats.Total-stats.Done, sps, errBuild.err)
			suggest := fmt.Sprintf("see %s for full command line and output", c.logFilename(c.outputLogFile, c.startDir))
			if c.sisoInfoLog != "" {
				suggest += fmt.Sprintf("\n or %s", c.logFilename(c.sisoInfoLog, c.startDir))
			}
			failedCommandsFile := c.logFilename(c.failedCommandsFile, "")
			if failedCommandsFile != "" {
				_, err := os.Stat(failedCommandsFile)
				if err == nil {
					suggest += fmt.Sprintf("\nuse %s to re-run failed commands", c.logFilename(c.failedCommandsFile, c.startDir))
				}
			}
			if ui.IsTerminal() {
				suggest = ui.SGR(ui.Bold, suggest)
			}
			ui.Default.Warningf("%s\n", suggest)
		default:
			msgPrefix := "Error"
			if ui.IsTerminal() {
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			}
			if status.Code(err) == codes.Unavailable {
				ui.Default.Errorf("\n%6s %s: could not connect to backend. If you want to build offline, pass `-o` or `--offline`\n %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
			} else {
				ui.Default.Errorf("\n%6s %s: %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, err)
			}
		}
		return subcommands.ExitFailure
	}
	msgPrefix := "Build Succeeded"
	if ui.IsTerminal() {
		dur = ui.SGR(ui.Bold, dur)
		msgPrefix = ui.SGR(ui.Green, msgPrefix)
	}
	ui.Default.Warningf("%6s %s: %d steps - %.02f/s\n", dur, msgPrefix, stats.Done-stats.Skipped, sps)
	return subcommands.ExitSuccess
}

// parse flags without stopping at non flags.
func parseFlagsFully(flagSet *flag.FlagSet) error {
	var targets []string
	for {
		args := flagSet.Args()
		if len(args) == 0 {
			break
		}
		argsRemaining := len(args)
		for i, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				targets = append(targets, arg)
				argsRemaining--
				continue
			}
			err := flagSet.Parse(args[i:])
			if err != nil {
				return err
			}
			break
		}
		if argsRemaining == 0 {
			break
		}
	}
	// targets are non-flags. set it to Args.
	return flagSet.Parse(targets)
}

type buildError struct {
	err error
}

func (b buildError) Error() string {
	return b.err.Error()
}

type flagError struct {
	err error
}

func (f flagError) Error() string {
	return f.err.Error()
}

var errNothingToDo = errors.New("nothing to do")

type errAlreadyLocked struct {
	err     error
	bufErr  error
	fname   string
	pidfile string
	owner   string
}

func (l errAlreadyLocked) Error() string {
	if l.bufErr != nil && l.pidfile != "" {
		return fmt.Sprintf("%s is locked, and failed to read %s: %v", l.fname, l.pidfile, l.bufErr)
	} else if l.bufErr != nil {
		return fmt.Sprintf("%s is locked, and failed to read: %v", l.fname, l.bufErr)
	}
	return fmt.Sprintf("%s is locked by %s: %v", l.fname, l.owner, l.err)
}
func (l errAlreadyLocked) Unwrap() error {
	if l.err != nil {
		return l.err
	}
	return l.bufErr
}

type errInterrupted struct{}

func (errInterrupted) Error() string        { return "interrupt by signal" }
func (errInterrupted) Is(target error) bool { return target == context.Canceled }

const (
	// relative to -state_dir
	failedTargetsFile = ".siso_failed_targets"
)

type batchFlag struct {
	c *Command
}

func (*batchFlag) IsBoolFlag() bool { return true }

func (f *batchFlag) String() string {
	if f == nil || f.c == nil {
		return "false"
	}
	return strconv.FormatBool(!f.c.fastNop && !f.c.fastLocal && !f.c.fastLastFailure && !f.c.fastExit)
}

func (f *batchFlag) Set(v string) error {
	if f == nil || f.c == nil {
		return errors.New("nil batchFlag")
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return err
	}
	f.c.fastNop = !b
	f.c.fastLocal = !b
	f.c.fastLastFailure = !b
	f.c.fastExit = !b
	return nil
}

func (c *Command) run(ctx context.Context) (stats build.Stats, err error) {
	// Cleanup functions to run after serial cleanups in parallel.
	// This mostly exists for logger and metrics functions cleanup.
	// Each of these functions take about 1 second on no-op builds to finish,
	// so to speed things up these 2 cleanups run in parallel, reducing 1 second or above from the build times.
	var pCleanups []func()

	defer func() {
		var wg sync.WaitGroup
		spin := ui.Default.NewSpinner()
		spin.Start("shutdown cloud logging/monitoring")
		for _, cleanup := range pCleanups {
			wg.Add(1)
			go func(cl func()) {
				defer wg.Done()
				cl()
			}(cleanup)
		}
		wg.Wait()
		spin.Stop(nil)
	}()

	ctx, cancel := context.WithCancelCause(ctx)
	defer signals.HandleInterrupt(ctx, func() {
		cancel(errInterrupted{})
	})()
	err = c.debugMode.check()
	if err != nil {
		return stats, flagError{err: err}
	}
	switch c.subtool {
	case "":
	case "list":
		return stats, flagError{
			err: errors.New(`ninja subtools:
  commands   Use "siso query commands" instead
  deps       Use "siso query deps" instead
  inputs     Use "siso query inputs" instead
  targets    Use "siso query targets" instead
  cleandead  clean built files that are no longer produced by the manifest`),
		}
	case "commands":
		return stats, flagError{
			err: errors.New("use `siso query commands` instead"),
		}
	case "deps":
		return stats, flagError{
			err: errors.New("use `siso query deps` instead"),
		}
	case "inputs":
		return stats, flagError{
			err: errors.New("use `siso query inputs` instead"),
		}
	case "targets":
		return stats, flagError{
			err: errors.New("use `siso query targets` instead"),
		}

	case "cleandead":
		c.cleandead = true
	default:
		return stats, flagError{err: fmt.Errorf("unknown tool %q", c.subtool)}
	}

	if c.ninjaJobs > 0 {
		ui.Default.Warningf("-j is deprecated. use -remote_jobs and -local_jobs instead\n")
	}
	if c.ninjaLoadLimit >= 0 {
		ui.Default.Warningf("-l is not supported.\n")
	}
	if c.failuresAllowed <= 0 {
		c.failuresAllowed = math.MaxInt
	}
	if c.failuresAllowed > 1 {
		c.fastLastFailure = false
	}

	if c.adjustWarn != "" {
		ui.Default.Warningf("-w is specified. but not supported. b/288807840\n")
	}

	var is_local_compile bool = false
	if c.offline {
		is_local_compile = true;
		if alex313031.IsNG() {
			ui.Default.Infof(ui.SGR(ui.Reset, "Offline mode\n"))
		} else {
			ui.Default.Warningf(ui.SGR(ui.Red, "offline mode\n"))
		}
		clog.Warningf(ctx, "offline mode")
		c.reopt = new(reapi.Option)
		c.reopt.Insecure = true
		c.projectID = ""
		c.enableCloudLogging = false
		c.enableResultstore = false
		c.enableCloudProfiler = false
		c.enableCloudTrace = false
		c.enableCloudMonitoring = false
		c.reproxyAddr = ""
	} else {
		if (alex313031.IsNG()) {
			ui.Default.Infof(ui.SGR(ui.Bold, "Online Mode\n"))
		}
	}

	if (alex313031.IsNG()) {
		ui.Default.Infof(ui.SGR(ui.Blue, "\"NG\" build config\n"))
	}

	execRoot, err := c.initWorkdirs(ctx)
	if err != nil {
		return stats, err
	}

	if c.stateDir != "." && c.fsopt.StateFile != "" {
		c.fsopt.StateFile = filepath.Join(c.stateDir, c.fsopt.StateFile)
	}
	lockFilename := filepath.Join(c.stateDir, ".siso_lock")
	if !c.dryRun {
		lock, err := newLockFile(ctx, lockFilename)
		switch {
		case errors.Is(err, errors.ErrUnsupported):
			clog.Warningf(ctx, "lockfile is not supported")
		case err != nil:
			return stats, err
		case err == nil:
			var owner string
			spin := ui.Default.NewSpinner()
			for {
				err = lock.Lock()
				alreadyLocked := &errAlreadyLocked{}
				if errors.As(err, &alreadyLocked) {
					if owner != alreadyLocked.owner {
						if owner != "" {
							spin.Done("lock holder %s completed", owner)
						}
						owner = alreadyLocked.owner
						spin.Start("waiting for lock holder %s..", owner)
					}
					select {
					case <-ctx.Done():
						return stats, context.Cause(ctx)
					case <-time.After(500 * time.Millisecond):
						continue
					}
				} else if err != nil {
					spin.Stop(err)
					return stats, err
				}
				if owner != "" {
					spin.Done("lock holder %s completed", owner)
				}
				break
			}
			defer func() {
				err := lock.Unlock()
				if err != nil {
					ui.Default.Errorf("failed to unlock %s: %v\n", lockFilename, err)
				}
				err = lock.Close()
				if err != nil {
					ui.Default.Errorf("failed to close %s: %v\n", lockFilename, err)
				}
			}()
		}
	}

	err = c.initLogDir(ctx)
	if err != nil {
		return stats, err
	}
	clog.Infof(ctx, "siso log dir=%s", c.logDir)

	resetCrashOutput, err := c.setupCrashOutput(ctx)
	if err != nil {
		return stats, err
	}
	defer resetCrashOutput()

	buildPath := build.NewPath(execRoot, c.dir)

	// compute default limits based on fstype of work dir (e.g. artfs),
	// not of exec root.
	limits := build.DefaultLimits(ctx)
	if c.localJobs > 0 || c.ninjaJobs > 0 {
		if c.ninjaJobs > 0 {
			limits.Local = c.ninjaJobs
		} else {
			limits.Local = c.localJobs
		}
		ui.Default.Infof("local_jobs = %d\n", limits.Local)
	}
	if c.remoteJobs == 0 && limits.Remote == 0 {
		is_local_compile = true
	}
	if c.remoteJobs > 0 && c.ninjaJobs == 0 {
		limits.Remote = c.remoteJobs
		limits.REWrap = c.remoteJobs
		ui.Default.Infof("remote_jobs = %d\n", limits.Remote)
	}
	if !c.fastLocal {
		limits.FastLocal = 0
		limits.StartLocal = 0
	}

	if is_local_compile {
		if alex313031.IsNG() {
			ui.Default.Infof(ui.SGR(ui.Reset, "is_local_compile = true\n"))
		}
	}

	if err = uuid.Validate(c.buildID); err != nil {
		return stats, flagError{err: fmt.Errorf("%q is an invalid build ID. -build_id must be a UUID", c.buildID)}
	}
	if len(c.jobID) > 1024 {
		return stats, flagError{err: fmt.Errorf("-job_id length must be less than 1024")}
	}

	projectID := c.reopt.UpdateProjectID(c.projectID)

	var credential cred.Cred
	if !c.offline && (c.reopt.NeedCred() || c.enableCloudLogging || c.enableResultstore || c.enableCloudProfiler || c.enableCloudTrace || c.enableCloudMonitoring) {
		// TODO: can be async until cred is needed?
		spin := ui.Default.NewSpinner()
		spin.Start("init credentials by %q", c.authOpts.Type)
		credential, err = cred.New(ctx, c.reopt.ServiceURI(), c.authOpts)
		if err != nil {
			spin.Stop(errors.New(""))
			return stats, err
		}
		spin.Stop(nil)
	}
	if c.enableCloudLogging {
		spin := ui.Default.NewSpinner()
		spin.Start("init cloud logging")
		logCtx, loggerURL, done, err := c.initCloudLogging(ctx, projectID, execRoot, credential)
		spin.Stop(err)
		if err != nil {
			// b/335295396 Compile step hitting write requests quota
			// rather than build fails, fallback to glog.
			ui.Default.Warningf("fallback to glog\n")
			c.enableCloudLogging = false
		} else {
			// use stderr for confirm no-op step. b/288534744
			ui.Default.Warningf("%s\n", loggerURL)
			pCleanups = append(pCleanups, done)
			ctx = logCtx
		}
	}
	// logging is ready.
	var properties resultstore.Properties
	properties.Add("dir", c.dir)
	info := cpuinfo()
	clog.Infof(ctx, "%s", info)
	properties.Add("cpu", info)
	info = gcinfo()
	clog.Infof(ctx, "%s", info)
	properties.Add("memgc", info)

	clog.Infof(ctx, "siso version %s", c.version)

	ver, err := version.Current()
	if err != nil {
		clog.Warningf(ctx, "version err: %v", err)
	} else if ver.CIPD != nil {
		clog.Infof(ctx, "CIPD package name: %s", ver.CIPD.PackageName)
		clog.Infof(ctx, "CIPD instance ID: %s", ver.CIPD.InstanceID)
		properties.Add("cipd_package_name", ver.CIPD.PackageName)
		properties.Add("cipd_instance_id", ver.CIPD.InstanceID)
	} else if ver.Build != nil {
		clog.Infof(ctx, "Go version: %s", ver.Build.GoVersion)
		properties.Add("go_version", ver.Build.GoVersion)
		clog.Infof(ctx, "module %s %s %s", ver.Build.Main.Path, ver.Build.Main.Version, ver.Build.Main.Sum)
		properties.Add("go_module_path", ver.Build.Main.Path)
		properties.Add("go_module_version", ver.Build.Main.Version)
		properties.Add("go_module_sum", ver.Build.Main.Sum)
		bs := ver.BuildSettings()
		for _, k := range slices.Sorted(maps.Keys(bs)) {
			v := bs[k]
			clog.Infof(ctx, "%s=%s", k, v)
			properties.Add(k, v)
		}
	}
	c.checkResourceLimits(ctx, limits)

	properties.Add("job_id", c.jobID)
	clog.Infof(ctx, "job id: %q", c.jobID)
	clog.Infof(ctx, "build id: %q", c.buildID)
	clog.Infof(ctx, "project id: %q", projectID)
	clog.Infof(ctx, "commandline %q", os.Args)
	clog.Infof(ctx, "is_terminal=%t fast_nop=%t fast_local=%t fast_last_failure=%t fast_exit=%t", ui.IsTerminal(), c.fastNop, c.fastLocal, c.fastLastFailure, c.fastExit)

	spin := ui.Default.NewSpinner()

	if c.enableResultstore {
		c.resultstoreUploader, err = resultstore.New(ctx, resultstore.Options{
			InvocationID:  c.buildID,
			Invocation:    c.invocation(ctx, c.buildID, projectID, execRoot, properties),
			ClientOptions: credential.ClientOptions(),
		})
		if err != nil {
			return stats, err
		}
		ui.Default.Warningf("https://btx.cloud.google.com/invocations/%s\n", c.buildID)
		defer func() {
			spin.Start("finishing upload to resultstore")
			exitCode := 0
			if err != nil {
				exitCode = 1
			}
			cerr := c.resultstoreUploader.Close(ctx, exitCode)
			if cerr != nil {
				clog.Warningf(ctx, "failed to close resultstore: %v", cerr)
			}
			spin.Stop(cerr)
		}()
	}
	if c.enableCloudProfiler {
		c.initCloudProfiler(ctx, projectID, credential)
	}
	metricsLabels := make(map[string]string)
	for l := range strings.SplitSeq(c.metricsLabels, ",") {
		kv := strings.Split(l, "=")
		if len(kv) != 2 {
			clog.Warningf(ctx, "metrics label must be in the form key=value. got %q", l)
			continue
		}
		metricsLabels[kv[0]] = kv[1]
	}
	if c.enableCloudMonitoring && c.reproxyAddr == "" {
		metricsProject := projectID
		if c.metricsProject != "" {
			metricsProject = c.metricsProject
		}
		e, err := c.initCloudMonitoring(ctx, credential, metricsProject, projectID, metricsLabels)
		if err != nil {
			return stats, err
		}
		// Export all the metrics before shutting down as we still need the cloud logger to be present.
		defer func() {
			// Report build metrics.
			var cacheHitRatio float64
			if stats.CacheHit+stats.Remote > 0 {
				cacheHitRatio = float64(stats.CacheHit) / float64(stats.CacheHit+stats.Remote)
			}
			isErr := err != nil && !errors.Is(err, errNothingToDo)
			monitoring.ExportBuildMetrics(ctx, time.Since(c.started), cacheHitRatio, isErr)
		}()
		pCleanups = append(pCleanups, func() {
			// Cloud logger is getting shut down in parallel, report locally.
			otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
				log.Warningf("failed to export to OpenTelemetry: %v", err)
			}))
			shutdownStart := time.Now()
			cerr := e.Shutdown(ctx)
			shutdownDuration := time.Since(shutdownStart)
			log.Infof("cloud monitoring shutdown took: %s", shutdownDuration)
			if cerr != nil {
				log.Warningf("failed to close Cloud monitoring exporter: %v", cerr)
			}
		})
	}
	var traceExporter *trace.Exporter
	if c.enableCloudTrace {
		traceExporter = c.initCloudTrace(ctx, projectID, credential)
		defer func() {
			closeStart := time.Now()
			traceExporter.Close(ctx)
			closeDuration := time.Since(closeStart)
			clog.Infof(ctx, "cloud trace shutdown took: %s", closeDuration)
		}()
	}
	// upload build pprof

	targets := c.Flags.Args()
	config, err := c.initConfig(ctx, execRoot, targets)
	if err != nil {
		return stats, err
	}

	failedTargetsFilename := filepath.Join(c.stateDir, failedTargetsFile)

	var eg errgroup.Group
	var localDepsLog *ninjautil.DepsLog
	eg.Go(func() error {
		depsLog, err := c.initDepsLog(ctx)
		if err != nil {
			return err
		}
		localDepsLog = depsLog
		return nil
	})

	if err := c.reopt.CheckValid(); err == nil {
		ui.Default.Infof(fmt.Sprintf("use %s\n", c.reopt))
	} else {
		if c.strictRemote {
			return stats, flagError{err: fmt.Errorf("no reapi specified, but remote is requested as --strict_remote: %w", err)}
		}
		if c.remoteJobs > 0 && c.reproxyAddr == "" {
			return stats, flagError{err: fmt.Errorf("no reapi specified, but remote is requested as --remote_jobs=%d: %w", c.remoteJobs, err)}
		}
	}
	ds, err := c.initDataSource(ctx, credential)
	if err != nil {
		return stats, err
	}
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			clog.Errorf(ctx, "close datasource: %v", err)
		}
	}()
	c.fsopt.DataSource = ds
	c.fsopt.OutputLocal, err = c.initOutputLocal()
	if err != nil {
		return stats, err
	}
	if c.logDir == "." || c.logDir == filepath.Join(execRoot, c.dir) {
		cwd := filepath.Join(execRoot, c.dir)
		// ignore siso files not to be captured by ReadDir
		// (i.g. scandeps for -I.)
		clog.Infof(ctx, "ignore siso files in %s", cwd)
		c.fsopt.Ignore = func(ctx context.Context, fname string) bool {
			dir, base := filepath.Split(fname)
			// allow siso prefix in other dir.
			// e.g. siso.gni exists in build/config/siso.
			if filepath.Clean(dir) != cwd {
				return false
			}
			if strings.HasPrefix(base, ".siso_") {
				return true
			}
			if strings.HasPrefix(base, "siso.") {
				return true
			}
			if strings.HasPrefix(base, "siso_") {
				return true
			}
			if base == ".ninja_log" {
				return true
			}
			return false
		}
	} else {
		// expect logDir is out of exec root.
		clog.Infof(ctx, "ignore .ninja_log")
		ninjaLogFname := filepath.Join(execRoot, c.dir, ".ninja_log")
		c.fsopt.Ignore = func(ctx context.Context, fname string) bool {
			return fname == ninjaLogFname
		}
	}
	cogfs, err := cogutil.New(ctx, execRoot)
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		clog.Warningf(ctx, "unable to use cog? %v", err)
	}
	if cogfs != nil {
		ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("build in cog: %s\n", cogfs.Info())))
		c.fsopt.CogFS = cogfs
	}
	if c.artfsDir != "" && c.artfsEndpoint != "" {
		artfs, err := artfsutil.New(ctx, c.artfsDir, c.artfsEndpoint)
		if err != nil {
			return stats, err
		}
		ui.Default.PrintLines(ui.SGR(ui.Yellow, "build on artfs\n"))
		c.fsopt.ArtFS = artfs
	}

	if fsmonitor := os.Getenv("SISO_FSMONITOR"); fsmonitor != "" {
		var fsmonitorPath string
		if !filepath.IsAbs(fsmonitor) {
			fsmonitorPath, err = exec.LookPath(fsmonitor)
			if err != nil {
				clog.Warningf(ctx, "failed to find fsmonitor %q: %v", fsmonitor, err)
				ui.Default.Warningf(ui.SGR(ui.BackgroundRed, fmt.Sprintf("SISO_FSMONITOR=%q: failed %v\n", fsmonitor, err)))
			}
		} else {
			fsmonitorPath = fsmonitor
		}
		if fsmonitorPath != "" {
			fsm := strings.TrimSuffix(filepath.Base(fsmonitor), filepath.Ext(fsmonitor))
			switch fsm {
			case "watchman":
				fsm, err := watchmanutil.New(ctx, fsmonitorPath, execRoot)
				if err != nil {
					clog.Warningf(ctx, "failed to initialize watchman: %v", err)
					ui.Default.Errorf(ui.SGR(ui.BackgroundRed, fmt.Sprintf("SISO_FSMONITOR=watchman: failed %v\n", err)))
				} else {
					ui.Default.Infof(ui.SGR(ui.Yellow, fmt.Sprintf("use watchman as fsmonitor: %s\n", fsmonitorPath)))
					c.fsopt.FSMonitor = fsm
				}
			default:
				ui.Default.Errorf(ui.SGR(ui.BackgroundRed, fmt.Sprintf("unknown SISO_FSMONITOR=%q (%q)\n", fsmonitor, fsm)))
			}
		}
	}

	spin.Start("loading fs state")

	hashFS, err := hashfs.New(ctx, *c.fsopt)
	spin.Stop(err)
	if err != nil {
		return stats, err
	}
	defer func() {
		if c.dryRun {
			return
		}
		if c.subtool != "" {
			// don't modify .siso_failed_targets by subtool
			return
		}
		if c.prepare {
			// don't modify .siso_failed_targets for prepare (ide query).
			return
		}
		if err != nil {
			// Even when batch mode, it records failed targets.
			// It will be read by Chromium recipe.
			var errBuild buildError
			if !errors.As(err, &errBuild) {
				return
			}
			var stepError build.StepError
			if !errors.As(errBuild.err, &stepError) {
				rerr := os.Remove(c.logFilename(c.failedCommandsFile, ""))
				if rerr != nil {
					clog.Warningf(ctx, "failed to remove failed command file: %v", rerr)
				}
				return
			}
			// store failed targets only when build steps failed.
			// i.e., don't store with error like context canceled, etc.
			clog.Infof(ctx, "record failed targets: %q", stepError.Target)
			serr := saveTargets(failedTargetsFilename, targets, []string{stepError.Target})
			if serr != nil {
				clog.Warningf(ctx, "failed to save failed targets: %v", serr)
				return
			}
		} else {
			rerr := os.Remove(c.logFilename(c.failedCommandsFile, ""))
			if rerr != nil {
				clog.Warningf(ctx, "failed to remove failed command file: %v", rerr)
			}
		}
	}()
	defer func() {
		hashFS.SetBuildTargets(ctx, targets, !c.dryRun && c.subtool == "" && !c.prepare && err == nil)
		err := hashFS.Close(ctx)
		if err != nil {
			clog.Errorf(ctx, "close hashfs: %v", err)
		}
	}()
	hashFSErr := hashFS.LoadErr()
	if hashFSErr != nil {
		ui.Default.Errorf(ui.SGR(ui.BackgroundRed, fmt.Sprintf("unable to do incremental build as fs state is corrupted: %v\n", hashFSErr)))
	}

	_, err = os.Stat(failedTargetsFilename)
	lastFailed := err == nil
	isClean := hashFS.IsClean(targets)
	clog.Infof(ctx, "hashfs loaderr: %v clean: %t (%q) last failed: %t", hashFSErr, isClean, targets, lastFailed)
	// In prepare mode for ide_query, it won't record .siso_failed_targets
	// and won't match with .siso_fs_state.
	// in this case, don't shortcut noop build, but better to check
	// build graph again.
	if !c.clobber && c.fastNop && !c.dryRun && !c.debugMode.Explain && c.subtool != "cleandead" && !c.prepare && hashFSErr == nil && isClean && !lastFailed {
		// TODO: better to check digest of .siso_fs_state?
		return stats, errNothingToDo
	}

	if c.resultstoreUploader != nil {
		defer func() {
			var ents []merkletree.Entry

			var files []string
			if c.metricsJSON != "" {
				files = append(files, c.metricsJSON)
			}
			// TODO(b/329564182): add other files? e.g. siso_output, siso_trace.json etc.
			if len(files) != 0 {
				var err error
				ents, err = hashFS.Entries(ctx, filepath.Join(execRoot, c.dir), files)
				if err != nil {
					clog.Warningf(ctx, "failed to get entries for %q: %v", files, err)
				}
			}
			ents = append(ents, merkletree.Entry{
				Name: "build.log",
				Data: c.resultstoreUploader.BuildLogData(),
			})
			spin.Start("uploading to resultstore")
			uerr := c.resultstoreUploader.UploadFiles(ctx, ents)
			if uerr != nil {
				clog.Warningf(ctx, "failed to upload results: %v", uerr)
			}
			spin.Stop(uerr)
		}()
	}

	bopts, done, err := c.initBuildOpts(ctx, projectID, buildPath, config, ds, hashFS, limits, traceExporter)
	if err != nil {
		return stats, err
	}
	defer done(&err)
	spin.Start("loading/recompacting deps log")
	err = eg.Wait()
	spin.Stop(err)
	if localDepsLog != nil {
		defer localDepsLog.Close()
	}
	// TODO(b/286501388): init concurrently for .siso_config/.siso_filegroups, build.ninja.
	if c.fsopt.KeepTainted {
		tainted := hashFS.TaintedFiles()
		if len(tainted) == 0 {
			ui.Default.Warningf(ui.SGR(ui.Yellow, "no tainted generated files:\n"))
		} else if len(tainted) < 5 {
			ui.Default.Warningf(ui.SGR(ui.Yellow, fmt.Sprintf("keep %d tainted files:\n %s\n", len(tainted), strings.Join(tainted, "\n "))))
		} else {
			ui.Default.Warningf(ui.SGR(ui.Yellow, fmt.Sprintf("keep %d tainted files:\n %s\n ...more\n", len(tainted), strings.Join(tainted, "\n "))))
		}
	}

	c.checkBuildNinja(ctx, buildPath, config, hashFS, localDepsLog, bopts)

	spin.Start("load siso config")
	stepConfig, err := ninjabuild.NewStepConfig(ctx, config, buildPath, hashFS, c.fname, c.stateDir)
	if err != nil {
		spin.Stop(err)
		return stats, err
	}
	spin.Stop(nil)
	spin.Start(fmt.Sprintf("load %s", c.fname))
	nstate, err := ninjabuild.Load(ctx, c.fname, buildPath)
	if err != nil {
		spin.Stop(errors.New(""))
		return stats, err
	}
	spin.Stop(nil)

	graph := ninjabuild.NewGraph(ctx, c.fname, nstate, config, buildPath, hashFS, stepConfig, localDepsLog)

	var lastFailedTargets []string
	if c.fastLastFailure && !c.clobber {
		lastFailedTargets, _ = checkTargets(ctx, failedTargetsFilename, targets)
		if len(lastFailedTargets) > 0 {
			bopts.LastFailureTargets = lastFailedTargets
			ui.Default.PrintLines(fmt.Sprintf(ui.SGR(ui.Yellow, "Prioritizing last failed targets: %s\n"), lastFailedTargets))
		}
	}
	err = os.Remove(failedTargetsFilename)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		clog.Warningf(ctx, "failed to remove %s: %v", failedTargetsFilename, err)
	}

	sisoMetadata := SisoMetadata{
		SisoVersion:   c.version,
		StartTime:     c.started,
		BuildID:       c.buildID,
		Targets:       targets,
		MetricsLabels: metricsLabels,
	}
	j, err := json.Marshal(sisoMetadata)
	if err != nil {
		return stats, err
	}
	if err := os.WriteFile(filepath.Join(c.logDir, sisoMetadataFilename), j, 0644); err != nil {
		return stats, err
	}

	return runNinja(ctx, c.fname, graph, bopts, targets, runNinjaOpts{
		cleandead:     c.cleandead,
		subtool:       c.subtool,
		enableStatusz: true,
	})
}

// checkBuildNinja quickly checks build.ninja is up-to-date by reading
// toplevel build.ninja file (without reading all build graph in subninja)
// and building build.ninja only.
// This optimizes for build.ninja generated by GN or so, which has build rule
// for build.ninja in main build.ninja file.
// Even if this assumption failed e.g. soong doesn't have such build rule,
// runNinja will rebuild manifest after reading all build.ninja files.
func (c *Command) checkBuildNinja(ctx context.Context, buildPath *build.Path, config *buildconfig.Config, hashFS *hashfs.HashFS, localDepsLog *ninjautil.DepsLog, bopts build.Options) {
	started := time.Now()
	defer func() {
		ui.Default.PrintLines("")
		clog.Infof(ctx, "check build ninja in %s", time.Since(started))
	}()
	nstate := ninjautil.NewState()
	p := ninjautil.NewManifestParser(nstate)
	err := p.LoadSingle(ctx, c.fname)
	if err != nil {
		clog.Warningf(ctx, "check build ninja: load %v", err)
		return
	}
	clog.Infof(ctx, "check build ninja: load file in %s", time.Since(started))
	// zero step config. no remote exec for gn gen?
	stepConfig := &ninjabuild.StepConfig{}
	graph := ninjabuild.NewGraph(ctx, c.fname, nstate, config, buildPath, hashFS, stepConfig, localDepsLog)

	err = rebuildManifest(ctx, graph, bopts)
	if errors.Is(err, build.ErrManifestModified) {
		started := time.Now()
		err := hashFS.Refresh(ctx, buildPath.ExecRoot)
		if err != nil {
			clog.Warningf(ctx, "%s modified. failed to refresh hashfs %s: %v", c.fname, time.Since(started), err)
			return
		}
		clog.Infof(ctx, "%s modifnied. refresh hashfs %s", c.fname, time.Since(started))
		return
	}
	if err != nil {
		// ignore failure: e.g. build.ninja target not defined
		clog.Warningf(ctx, "check build ninja: rebuild manifest %v", err)
		return
	}
}

type runNinjaOpts struct {
	// whether to perform cleandead or not.
	cleandead bool

	// subtool name.
	// if "cleandead", it returns after cleandead performed.
	subtool string

	// enable statusz (for `siso ps`)
	enableStatusz bool
}

func runNinja(ctx context.Context, fname string, graph *ninjabuild.Graph, bopts build.Options, targets []string, nopts runNinjaOpts) (build.Stats, error) {
	spin := ui.Default.NewSpinner()

	builddir := graph.Binding("builddir")
	clog.Infof(ctx, "builddir=%q", builddir)
	if builddir != "" {
		err := os.MkdirAll(builddir, 0755)
		if err != nil {
			return build.Stats{}, err
		}
	}
	ninjaLogWriter, err := ninjautil.InitializeNinjaLog(builddir)
	if err != nil {
		return build.Stats{}, err
	}
	defer func() {
		cerr := ninjaLogWriter.Close()
		clog.Infof(ctx, "close .ninja_log: %v", cerr)
	}()
	bopts.NinjaLogWriter = ninjaLogWriter

	for {
		clog.Infof(ctx, "build starts")
		stats, err := doBuild(ctx, graph, bopts, nopts, targets...)
		if errors.Is(err, build.ErrManifestModified) {
			if bopts.DryRun {
				return stats, nil
			}
			clog.Infof(ctx, "%s modified", fname)
			spin.Start("reloading")
			err := graph.Reload(ctx)
			if err != nil {
				spin.Stop(err)
				return stats, err
			}
			spin.Stop(nil)
			clog.Infof(ctx, "reload done. build retry")
			continue
		}
		clog.Infof(ctx, "build finished: %v", err)
		if bopts.ResultstoreUploader != nil {
			if err != nil {
				bopts.ResultstoreUploader.AddBuildLog(fmt.Sprintf("build failed: %v\n", err))
			} else {
				bopts.ResultstoreUploader.AddBuildLog("build succeeded\n")
			}
		}
		return stats, err
	}
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	// TODO(b/340381100): extract common flags for ninja commands
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory")
	flagSet.StringVar(&c.configName, "config", "", "config name passed to starlark")
	flagSet.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	defaultBuildID := os.Getenv("SISO_BUILD_ID")
	if defaultBuildID == "" {
		defaultBuildID = uuid.New().String()
	}
	flagSet.StringVar(&c.buildID, "build_id", defaultBuildID, "ID for the build. used for `invocation_id` of remote-apis-sdks and `tool_invocation_id` of remote-apis, and Cloud logging resource `build_id` label.")
	flagSet.StringVar(&c.jobID, "job_id", uuid.New().String(), "ID for a grouping of related builds such as a Buildbucket job. used for `correlated_invocations_id` of remote-apis and remote-apis-sdks, and Cloud logging resource `job_id` label.")

	flagSet.BoolVar(&c.offline, "offline", false, "offline mode.")
	flagSet.BoolVar(&c.offline, "o", false, "alias of `-offline`")
	if f := flagSet.Lookup("offline"); f != nil {
		if s := os.Getenv("RBE_remote_disabled"); s != "" {
			err := f.Value.Set(s)
			if err != nil {
				log.Errorf("invalid RBE_remote_disabled=%q: %v", s, err)
			}
		}
	}

	flagSet.BoolVar(&c.fastNop, "fast_nop", ui.IsTerminal(), "enable fast nop check")
	flagSet.BoolVar(&c.fastLocal, "fast_local", ui.IsTerminal(), "enable fast local")
	flagSet.BoolVar(&c.fastLastFailure, "fast_last_failure", ui.IsTerminal(), "enable fast last failure check")
	flagSet.BoolVar(&c.fastExit, "fast_exit", ui.IsTerminal(), "enable fast exit")
	batch := &batchFlag{c: c}
	flagSet.Var(batch, "batch", "batch mode. prefer thoughput over low latency for build failures. disable -fast_nop, -fast_local -fast_last_failure -fast_exit")

	flagSet.BoolVar(&c.quiet, "quiet", false, "don't show progress status, just command output")
	flagSet.BoolVar(&c.verbose, "verbose", false, "show all command lines while building")
	flagSet.BoolVar(&c.verbose, "v", false, "show all command lines while building (alias of --verbose)")
	flagSet.BoolVar(&c.verboseFailures, "verbose_failures", true, "show failed command lines")
	flagSet.BoolVar(&c.dryRun, "n", false, "dry run")
	flagSet.BoolVar(&c.clobber, "clobber", false, "clobber build")
	flagSet.BoolVar(&c.prepare, "prepare", false, "build inputs of targets, but not build target itself.")
	flagSet.BoolVar(&c.strictRemote, "strict_remote", false, "don't use local for remote step. i.e. no fastlocal, no local fallback")
	flagSet.IntVar(&c.failuresAllowed, "k", 1, "keep going until N jobs fail (0 means inifinity)")
	flagSet.StringVar(&c.actionSalt, "action_salt", "", "action salt")

	flagSet.IntVar(&c.ninjaJobs, "j", 0, "Deprecated. use -remote_jobs and -local_jobs instead")
	flagSet.IntVar(&c.ninjaLoadLimit, "l", -1, "not supported.")
	flagSet.IntVar(&c.localJobs, "local_jobs", 0, "run N local jobs in parallel. when the value is no positive, the default will be computed based on # of CPUs.")
	flagSet.IntVar(&c.remoteJobs, "remote_jobs", 0, "run N remote jobs in parallel. when the value is no positive, the default will be computed based on # of CPUs.")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build manifest filename (relative to -C)")

	flagSet.StringVar(&c.cacheDir, "cache_dir", defaultCacheDir(), "cache directory")
	flagSet.BoolVar(&c.localCacheEnable, "local_cache_enable", false, "local cache enable")
	flagSet.BoolVar(&c.cacheEnableRead, "cache_enable_read", true, "cache enable read")

	flagSet.StringVar(&c.configRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
	flagSet.StringVar(&c.configFilename, "load", "@config//main.star", "config filename (@config// is --config_repo_dir)")
	flagSet.StringVar(&c.outputLocalStrategy, "output_local_strategy", "full", `strategy for output_local. "full": download all outputs. "greedy": downloads most outputs except intermediate objs. "minimum": downloads as few as possible`)
	flagSet.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C, -state_dir)")

	flagSet.StringVar(&c.stateDir, "state_dir", "", "state directory (relative to -C) [default: same dir as build.ninja]")

	flagSet.StringVar(&c.logDir, "log_dir", "", "log directory (relative to -C) [default: same dir as build.ninja]")

	// https://android.googlesource.com/platform/build/soong/+/refs/heads/main/ui/build/ninja.go
	flagSet.StringVar(&c.frontendFile, "frontend_file", "", "frontend FIFO file to report build status to soong ui, or `-` to report to stdout.")

	flagSet.StringVar(&c.failureSummaryFile, "failure_summary", "", "filename for failure summary (relative to -log_dir)")
	c.failedCommandsFile = "siso_failed_commands.sh"
	if runtime.GOOS == "windows" {
		c.failedCommandsFile = "siso_failed_commands.bat"
	}
	flagSet.StringVar(&c.failedCommandsFile, "failed_commands", c.failedCommandsFile, "script file to rerun the last failed commands")
	flagSet.StringVar(&c.outputLogFile, "output_log", "siso_output", "output log filename (relative to -log_dir")
	flagSet.StringVar(&c.explainFile, "explain_log", "siso_explain", "explain log filename (relative to -log_dir")
	flagSet.StringVar(&c.localexecLogFile, "localexec_log", "siso_localexec", "localexec log filename (relative to -log_dir")
	flagSet.StringVar(&c.metricsJSON, "metrics_json", "siso_metrics.json", "metrics JSON filename (relative to -log_dir)")
	flagSet.StringVar(&c.traceJSON, "trace_json", "siso_trace.json", "trace JSON filename (relative to -log_dir)")
	flagSet.StringVar(&c.buildPprof, "build_pprof", "siso_build.pprof", "build pprof filename (relative to -log_dir)")

	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(flagSet)

	c.reopt = new(reapi.Option)
	c.reopt.RegisterFlags(flagSet, reapi.Envs("REAPI"))
	flagSet.BoolVar(&c.reExecEnable, "re_exec_enable", true, "remote exec enable")
	flagSet.BoolVar(&c.reCacheEnableRead, "re_cache_enable_read", true, "remote exec cache enable read")
	flagSet.BoolVar(&c.reCacheEnableWrite, "re_cache_enable_write", false, "remote exec cache allow local trusted uploads")
	// reclient_helper.py sets the RBE_server_address
	// https://chromium.googlesource.com/chromium/tools/depot_tools.git/+/e13840bd9a04f464e3bef22afac1976fc15a96a0/reclient_helper.py#138
	c.reproxyAddr = os.Getenv("RBE_server_address")

	flagSet.StringVar(&c.artfsDir, "artfs_dir", "", "artfs mount point")
	flagSet.StringVar(&c.artfsEndpoint, "artfs_endpoint", "localhost:65001", "artfs server endpoint")

	flagSet.DurationVar(&c.traceThreshold, "trace_threshold", 1*time.Minute, "threshold for trace record")
	flagSet.DurationVar(&c.traceSpanThreshold, "trace_span_threshold", 100*time.Millisecond, "theshold for trace span record")

	flagSet.BoolVar(&c.enableCloudLogging, "enable_cloud_logging", false, "enable cloud logging")
	flagSet.BoolVar(&c.enableResultstore, "enable_resultstore", false, "enable resultstore")
	flagSet.BoolVar(&c.enableCloudProfiler, "enable_cloud_profiler", false, "enable cloud profiler")
	flagSet.StringVar(&c.cloudProfilerServiceName, "cloud_profiler_service_name", "siso", "cloud profiler service name")
	flagSet.BoolVar(&c.enableCloudTrace, "enable_cloud_trace", false, "enable cloud trace")
	flagSet.BoolVar(&c.enableCloudMonitoring, "enable_cloud_monitoring", false, "enable cloud monitoring")
	flagSet.BoolVar(&c.enableBuildNinjaFilesUpload, "enable_build_ninja_files_upload", true, "enable Build Ninja files upload to RBE-CAS")
	flagSet.StringVar(&c.metricsLabels, "metrics_labels", os.Getenv("RBE_metrics_labels"), "comma-separated arbitrary key value pairs in the form key=value, which are added to cloud monitoring metrics and siso_metadata.json.")
	flagSet.StringVar(&c.metricsProject, "metrics_project", os.Getenv("RBE_metrics_project"), "override Cloud Monitoring GCP project where Siso sends action and build metrics.")

	flagSet.StringVar(&c.subtool, "t", "", "run a subtool (use '-t list' to list subtools)")
	flagSet.BoolVar(&c.cleandead, "cleandead", false, "clean built files that are no longer produced by the manifest")
	flagSet.Var(&c.debugMode, "d", "enable debugging (use '-d list' to list modes)")
	flagSet.StringVar(&c.adjustWarn, "w", "", "adjust warnings. not supported b/288807840")
}

func (c *Command) initWorkdirs(ctx context.Context) (string, error) {
	// don't use $PWD for current directory
	// to avoid symlink issue. b/286779149
	pwd := os.Getenv("PWD")
	_ = os.Unsetenv("PWD") // no error for safe env key name.

	execRoot, err := os.Getwd()
	if pwd != "" {
		_ = os.Setenv("PWD", pwd) // no error to reset env with valid value.
	}
	if err != nil {
		return "", err
	}
	c.startDir = execRoot
	clog.Infof(ctx, "wd: %s", execRoot)
	// The formatting of this string, complete with funny quotes, is
	// so Emacs can properly identify that the cwd has changed for
	// subsequent commands.
	// Don't print this if a tool is being used, so that tool output
	// can be piped into a file without this string showing up.
	if c.subtool == "" && c.dir != "." {
		ui.Default.PrintLines(fmt.Sprintf("ninja: Entering directory `%s'\n\n", c.dir))
	}
	err = os.Chdir(c.dir)
	if err != nil {
		return "", err
	}
	clog.Infof(ctx, "change dir to %s", c.dir)
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	realCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		clog.Warningf(ctx, "failed to eval symlinks %q: %v", cwd, err)
	} else if cwd != realCWD {
		clog.Infof(ctx, "cwd %s -> %s", cwd, realCWD)
		cwd = realCWD
	}
	if !filepath.IsAbs(c.configRepoDir) {
		execRoot, err = build.DetectExecRoot(cwd, c.configRepoDir)
		if err != nil {
			return "", err
		}
		c.configRepoDir = filepath.Join(execRoot, c.configRepoDir)
	}
	clog.Infof(ctx, "exec_root: %s", execRoot)

	// recalculate dir as relative to exec_root.
	// recipe may use absolute path for -C.
	rdir, err := filepath.Rel(execRoot, cwd)
	if err != nil {
		return "", err
	}
	if !filepath.IsLocal(rdir) {
		return "", fmt.Errorf("dir %q is out of exec root %q", cwd, execRoot)
	}
	c.dir = rdir
	clog.Infof(ctx, "working_directory in exec_root: %s", c.dir)
	if c.startDir != execRoot {
		ui.Default.Infof("exec_root=%s dir=%s\n", execRoot, c.dir)
	}
	_, err = os.Stat(c.fname)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s not found in %s. need `-C <dir>`?", c.fname, cwd)
	}
	return execRoot, err
}

func (c *Command) initCloudLogging(ctx context.Context, projectID, execRoot string, credential cred.Cred) (context.Context, string, func(), error) {
	log.Infof("enable cloud logging project=%s id=%s", projectID, c.buildID)

	// log_id: "siso.log" and "siso.step"
	// use generic_task resource
	// https://cloud.google.com/logging/docs/api/v2/resource-list
	// https://cloud.google.com/monitoring/api/resources#tag_generic_task
	// Use a switchable logger to avoid data race.
	// The race happens between `logging.NewClient()` which may start using
	// the logger in background goroutines, and `grpclog.SetLoggerV2()`
	// which sets the logger.
	// `grpclog.SetLoggerV2` is not thread-safe and should only be called once at init time.
	slogger := clog.NewSwitchableGRPCLogger()
	grpclog.SetLoggerV2(slogger)
	client, err := logging.NewClient(ctx, projectID, credential.ClientOptions()...)
	if err != nil {
		return ctx, "", func() {}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return ctx, "", func() {}, err
	}
	logger, err := clog.New(ctx, client, "siso.log", "siso.step", &mrpb.MonitoredResource{
		Type: "generic_task",
		// should set labels for generic_task.
		// see https://cloud.google.com/logging/docs/api/v2/resource-list
		Labels: map[string]string{
			"project_id": projectID,
			"job":        c.jobID,
			"task_id":    c.buildID,
			"location":   hostname,
			"namespace":  execRoot,
		},
	})
	if err != nil {
		return ctx, "", func() {}, err
	}
	ctx = clog.NewContext(ctx, logger)
	slogger.SetLogger(logger)
	return ctx, logger.URL(), func() {
		errch := make(chan error, 1)
		closeStart := time.Now()
		go func() {
			errch <- logger.Close()
		}()
		timeout := 1 * time.Second
		if !c.fastExit {
			timeout = 10 * time.Second
		}
		// Don't use clog as it's closing Cloud logging client.
		select {
		case <-time.After(timeout):
			log.Warningf("close not finished in %s", timeout)
		case err := <-errch:
			if err != nil {
				log.Warningf("falied to close Cloud logger: %v", err)
			} else {
				log.Infof("cloud logging shutdown took: %s", time.Since(closeStart))
			}
		}
	}, nil
}

func (c *Command) initCloudProfiler(ctx context.Context, projectID string, credential cred.Cred) {
	clog.Infof(ctx, "enable cloud profiler %q in %s", c.cloudProfilerServiceName, projectID)
	config := profiler.Config{
		Service:        c.cloudProfilerServiceName,
		ServiceVersion: fmt.Sprintf("%s/%s", c.version, runtime.GOOS),
		MutexProfiling: true,
		ProjectID:      projectID,
	}
	if metadata.OnGCE() {
		// need to set zone,instance if it seems to run on GCE
		// but metadata failed to reply them. b/376372151
		var err error
		config.Zone, err = metadata.ZoneWithContext(ctx)
		if err != nil {
			clog.Warningf(ctx, "failed to get zone from metadata: %v", err)
			config.Zone = "us-central1-a"
		}
		config.Instance, err = metadata.InstanceNameWithContext(ctx)
		if err != nil {
			clog.Warningf(ctx, "failed to get instnace from metadata: %v", err)
			config.Instance = "non-gce-instance"
		}
	}
	err := profiler.Start(config, credential.ClientOptions()...)
	if err != nil {
		clog.Errorf(ctx, "failed to start cloud profiler: %v", err)
	}
}

func (c *Command) initCloudTrace(ctx context.Context, projectID string, credential cred.Cred) *trace.Exporter {
	clog.Infof(ctx, "enable trace in %s [trace > %s]", projectID, c.traceThreshold)
	traceExporter, err := trace.NewExporter(ctx, trace.Options{
		ProjectID:     projectID,
		ServiceName:   fmt.Sprintf("siso/%s/%s", c.version, runtime.GOOS),
		StepThreshold: c.traceThreshold,
		SpanThreshold: c.traceSpanThreshold,
		ClientOptions: slices.Clone(credential.ClientOptions()),
	})
	if err != nil {
		clog.Errorf(ctx, "failed to start trace exporter: %v", err)
	}
	return traceExporter
}

func (c *Command) initCloudMonitoring(ctx context.Context, credential cred.Cred, metricsProject, rbeProjectID string, labels map[string]string) (*smetric.MeterProvider, error) {
	clog.Infof(ctx, "enable cloud monitoring in %s", metricsProject)
	views, err := monitoring.SetupViews(ctx, c.version, rbeProjectID, labels)
	if err != nil {
		return nil, err
	}
	exporter, err := cloudmetric.New(
		cloudmetric.WithProjectID(metricsProject),
		cloudmetric.WithMonitoringClientOptions(credential.ClientOptions()...),
		cloudmetric.WithMetricDescriptorTypeFormatter(func(metrics metricdata.Metrics) string {
			return fmt.Sprintf("workload.googleapis.com/siso/%s", metrics.Name)
		}),
	)
	if err != nil {
		return nil, err
	}
	mp, err := monitoring.NewMetricProvider(ctx, rbeProjectID, exporter, views)
	if err != nil {
		return nil, err
	}
	otel.SetMeterProvider(mp)
	clog.Infof(ctx, "OpenTelemetry exporter has started in %q for RBE project %q", metricsProject, rbeProjectID)
	return mp, nil
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

func (c *Command) initFlags(targets []string) map[string]string {
	flags := make(map[string]string)
	c.Flags.Visit(func(f *flag.Flag) {
		name := f.Name
		if name == "C" {
			name = "dir"
		}
		flags[name] = f.Value.String()
	})
	flags["project"] = c.projectID
	flags["is_terminal"] = strconv.FormatBool(ui.IsTerminal())
	flags["targets"] = strings.Join(targets, " ")
	return flags
}

func (c *Command) initConfig(ctx context.Context, execRoot string, targets []string) (*buildconfig.Config, error) {
	if c.configFilename == "" {
		return nil, errors.New("no config filename")
	}
	cfgrepos := map[string]fs.FS{
		"config":           os.DirFS(c.configRepoDir),
		"config_overrides": os.DirFS(filepath.Join(execRoot, ".siso_remote")),
	}
	flags := c.initFlags(targets)
	config, err := buildconfig.New(ctx, c.configFilename, flags, cfgrepos)
	if err != nil {
		return nil, err
	}
	if gnArgs, err := os.ReadFile("args.gn"); err == nil {
		err := config.Metadata.Set("args.gn", string(gnArgs))
		if err != nil {
			return nil, err
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		clog.Warningf(ctx, "no args.gn: %v", err)
	} else {
		return nil, err
	}
	return config, nil
}

func (c *Command) initDepsLog(ctx context.Context) (*ninjautil.DepsLog, error) {
	depsLogFile := filepath.Join(c.stateDir, c.depsLogFile)
	err := os.MkdirAll(filepath.Dir(depsLogFile), 0755)
	if err != nil {
		clog.Warningf(ctx, "failed to mkdir for deps log: %v", err)
		return nil, err
	}
	depsLog, err := ninjautil.NewDepsLog(ctx, depsLogFile)
	if err != nil {
		clog.Warningf(ctx, "failed to load deps log: %v", err)
		return nil, err
	}
	if !depsLog.NeedsRecompact() {
		return depsLog, nil
	}
	err = depsLog.Recompact(ctx)
	if err != nil {
		clog.Warningf(ctx, "failed to recompact deps log: %v", err)
		return nil, err
	}
	return depsLog, nil
}

func (c *Command) initBuildOpts(ctx context.Context, projectID string, buildPath *build.Path, config *buildconfig.Config, ds dataSource, hashFS *hashfs.HashFS, limits build.Limits, traceExporter *trace.Exporter) (bopts build.Options, done func(*error), err error) {
	var dones []func(*error)
	defer func() {
		if err != nil {
			for i := len(dones) - 1; i >= 0; i++ {
				dones[i](&err)
			}
			dones = nil
		}
	}()

	failureSummaryWriter, done, err := c.logWriter(ctx, c.failureSummaryFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)
	dones = append(dones, func(errp *error) {
		if failureSummaryWriter != nil && *errp != nil {
			fmt.Fprintf(failureSummaryWriter, "error: %v\n", *errp)
		}
	})
	failedCommandsWriter, done, err := c.logWriter(ctx, c.failedCommandsFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)
	newline := "\n"
	if runtime.GOOS != "windows" {
		if f, ok := failedCommandsWriter.(*os.File); ok {
			err = f.Chmod(0755)
			if err != nil {
				return bopts, nil, err
			}
		}
		fmt.Fprintf(failedCommandsWriter, "#!/bin/sh\n")
		fmt.Fprintf(failedCommandsWriter, "set -ve\n")
	} else {
		newline = "\r\n"
	}
	fmt.Fprintf(failedCommandsWriter, "cd %s%s", filepath.Join(buildPath.ExecRoot, buildPath.Dir), newline)
	// TODO: for reproxy mode, may need to run reproxy for rewrapper commands.

	outputLogWriter, done, err := c.logWriter(ctx, c.outputLogFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)
	explainWriter, done, err := c.logWriter(ctx, c.explainFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)
	if c.debugMode.Explain {
		if explainWriter == nil {
			explainWriter = newExplainWriter(os.Stderr, "")
		} else {
			explainWriter = io.MultiWriter(newExplainWriter(os.Stderr, filepath.Join(c.dir, c.explainFile)), explainWriter)
		}
	}

	localexecLogWriter, done, err := c.logWriter(ctx, c.localexecLogFile)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)

	metricsJSONWriter, done, err := c.logWriter(ctx, c.metricsJSON)
	if err != nil {
		return bopts, nil, err
	}
	dones = append(dones, done)

	if !filepath.IsAbs(c.traceJSON) {
		c.traceJSON = filepath.Join(c.logDir, c.traceJSON)
	}
	if !filepath.IsAbs(c.buildPprof) {
		c.buildPprof = filepath.Join(c.logDir, c.buildPprof)
	}

	var actionSaltBytes []byte
	if c.actionSalt != "" {
		actionSaltBytes = []byte(c.actionSalt)
	}
	if c.traceJSON != "" {
		rotateFiles(ctx, c.traceJSON)
	}

	cache, err := build.NewCache(ctx, build.CacheOptions{
		Store:      ds.cache,
		EnableRead: c.cacheEnableRead,
	})
	if err != nil {
		clog.Warningf(ctx, "no cache enabled: %v", err)
	}
	bopts = build.Options{
		JobID:                 c.jobID,
		ID:                    c.buildID,
		StartTime:             c.started,
		ProjectID:             projectID,
		Metadata:              config.Metadata,
		Path:                  buildPath,
		HashFS:                hashFS,
		REAPIClient:           ds.client,
		REExecEnable:          c.reExecEnable,
		RECacheEnableRead:     c.reCacheEnableRead,
		RECacheEnableWrite:    c.reCacheEnableWrite,
		ReproxyAddr:           c.reproxyAddr,
		ActionSalt:            actionSaltBytes,
		OutputLocal:           build.OutputLocalFunc(c.fsopt.OutputLocal),
		Cache:                 cache,
		FailureSummaryWriter:  failureSummaryWriter,
		FailedCommandsWriter:  failedCommandsWriter,
		OutputLogWriter:       outputLogWriter,
		ExplainWriter:         explainWriter,
		LocalexecLogWriter:    localexecLogWriter,
		MetricsJSONWriter:     metricsJSONWriter,
		TraceExporter:         traceExporter,
		TraceJSON:             c.traceJSON,
		Pprof:                 c.buildPprof,
		ResultstoreUploader:   c.resultstoreUploader,
		Clobber:               c.clobber,
		FastExit:              c.fastExit,
		Prepare:               c.prepare,
		Verbose:               c.verbose,
		VerboseFailures:       c.verboseFailures,
		DryRun:                c.dryRun,
		StrictRemote:          c.strictRemote,
		FailuresAllowed:       c.failuresAllowed,
		KeepRSP:               c.debugMode.Keeprsp,
		KeepDepfile:           c.debugMode.Keepdepfile,
		Limits:                limits,
		UploadBuildNinjaFiles: c.enableBuildNinjaFilesUpload,
	}
	return bopts, func(err *error) {
		for i := len(dones) - 1; i >= 0; i-- {
			dones[i](err)
		}
	}, nil
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

// glogFilename returns filename of glog logfile. i.e. siso.INFO.
func (c *Command) glogFilename() string {
	logFilename := "siso.INFO"
	if runtime.GOOS == "windows" {
		logFilename = "siso.exe.INFO"
	}
	return filepath.Join(c.logDir, logFilename)
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

func defaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		log.Warningf("Failed to get user cache dir: %v", err)
		return ""
	}
	return filepath.Join(d, "siso")
}

func rebuildManifest(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options) error {
	_, err := graph.Targets(ctx, graph.Filename())
	if err != nil {
		clog.Warningf(ctx, "don't rebuild manifest: no target for %s: %v", graph.Filename(), err)
		return nil
	}
	clog.Infof(ctx, "rebuild manifest")
	mfbopts := bopts
	mfbopts.Clobber = false
	mfbopts.Prepare = false
	mfbopts.RebuildManifest = graph.Filename()
	mfb, err := build.New(ctx, graph, mfbopts)
	if err != nil {
		return err
	}

	err = mfb.Build(ctx, "rebuild manifest", graph.Filename())
	cerr := mfb.Close()
	if cerr != nil {
		return fmt.Errorf("failed to close builder: %w", cerr)
	}
	return err
}

func doBuild(ctx context.Context, graph *ninjabuild.Graph, bopts build.Options, nopts runNinjaOpts, args ...string) (stats build.Stats, err error) {
	err = rebuildManifest(ctx, graph, bopts)
	if err != nil {
		return stats, err
	}
	stateDir := graph.StateDir()
	if bopts.ResultstoreUploader != nil {
		err := bopts.ResultstoreUploader.NewConfiguration(ctx, "default", graph.ConfigProperties())
		if err != nil {
			return stats, err
		}
		bopts.ResultstoreUploader.HashFS = bopts.HashFS
		bopts.ResultstoreUploader.REAPIClient = bopts.REAPIClient

		ents, err := bopts.HashFS.Entries(ctx, filepath.Join(bopts.Path.ExecRoot, bopts.Path.Dir), []string{filepath.Join(stateDir, ".siso_config"), filepath.Join(stateDir, ".siso_filegroups")})
		if err != nil {
			return stats, err
		}
		err = bopts.ResultstoreUploader.UploadFiles(ctx, ents)
		if err != nil {
			return stats, err
		}
	}

	if !bopts.DryRun && nopts.cleandead {
		spin := ui.Default.NewSpinner()
		spin.Start("cleaning deadfiles")
		n, total, err := graph.CleanDead(ctx)
		if err != nil {
			spin.Stop(err)
			return stats, err
		}
		if nopts.subtool == "cleandead" {
			spin.Done("%d/%d generated files", n, total)
			return stats, nil
		}
		spin.Stop(nil)
	}

	b, err := build.New(ctx, graph, bopts)
	if err != nil {
		return stats, err
	}
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if nopts.enableStatusz {
		go func() {
			err := newStatuszServer(hctx, b, stateDir)
			if err != nil {
				clog.Warningf(ctx, "statusz: %v", err)
			}
		}()
	}

	defer func(ctx context.Context) {
		cerr := b.Close()
		if cerr != nil {
			clog.Warningf(ctx, "failed to close builder: %v", cerr)
		}
	}(ctx)
	// prof := newCPUProfiler(ctx, "build")
	err = b.Build(ctx, "build", args...)
	// prof.stop(ctx)

	if err != nil {
		if errors.As(err, &build.MissingSourceError{}) {
			return stats, err
		}
		if errors.As(err, &build.DependencyCycleError{}) {
			return stats, err
		}
	}

	semaTraces := make(map[string]semaTrace)
	tstats := b.TraceStats()
	var rbeWorker, rbeExec *build.TraceStat
	for _, ts := range tstats {
		clog.Infof(ctx, "%s: n=%d avg=%s max=%s", ts.Name, ts.N, ts.Avg(), ts.Max)
		switch {
		case strings.HasPrefix(ts.Name, "wait:"):
			name := strings.TrimPrefix(ts.Name, "wait:")
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.waitAvg = ts.Avg()
			t.waitBuckets = ts.Buckets
			semaTraces[name] = t
		case strings.HasPrefix(ts.Name, "serv:"):
			name := strings.TrimPrefix(ts.Name, "serv:")
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.nerr = ts.NErr
			t.servAvg = ts.Avg()
			t.servBuckets = ts.Buckets
			semaTraces[name] = t
		case ts.Name == "rbe:queue":
			name := "rbe:sched"
			t := semaTraces[name]
			t.name = name
			t.n = ts.N
			t.nerr = ts.NErr
			t.waitAvg = ts.Avg()
			t.waitBuckets = ts.Buckets
			semaTraces[name] = t
		case ts.Name == "rbe:worker":
			rbeWorker = ts
		case ts.Name == "rbe:exec":
			rbeExec = ts
		}
	}
	if rbeWorker != nil {
		name := "rbe:sched"
		t := semaTraces[name]
		t.name = name
		t.servAvg = rbeWorker.Avg()
		t.servBuckets = rbeWorker.Buckets
		semaTraces[name] = t
	}
	if rbeWorker != nil && rbeExec != nil {
		name := "rbe:worker"
		t := semaTraces[name]
		t.name = name
		t.n = rbeExec.N
		t.waitAvg = rbeWorker.Avg() - rbeExec.Avg()
		// number of waits would not be correct with this calculation
		// because it just uses counts in buckets.
		// not sure how we can measure actual waiting time in buckets,
		// but this would provide enough estimated values.
		for i := range rbeWorker.Buckets {
			t.waitBuckets[i] = rbeWorker.Buckets[i] - rbeExec.Buckets[i]
		}
		t.servAvg = rbeExec.Avg()
		t.servBuckets = rbeExec.Buckets
		semaTraces[name] = t
	}
	if len(semaTraces) > 0 {
		rut := dumpResourceUsageTable(semaTraces)
		clog.Infof(ctx, "resource usage table:\n%s", rut)
		if bopts.ResultstoreUploader != nil {
			bopts.ResultstoreUploader.AddBuildLog(rut + "\n")
		}
	}
	stats = b.Stats()
	clog.Infof(ctx, "stats=%#v", stats)
	if err != nil {
		return stats, buildError{err: err}
	}
	if bopts.REAPIClient == nil {
		return stats, err
	}
	// TODO(b/266518906): wait for completion of uploading manifest
	return stats, err
}

func dumpResourceUsageTable(semaTraces map[string]semaTrace) string {
	var semaNames []string
	for key := range semaTraces {
		semaNames = append(semaNames, key)
	}
	sort.Strings(semaNames)
	var lsb, usb strings.Builder
	var needToShow bool
	ltw := tabwriter.NewWriter(&lsb, 10, 8, 1, ' ', tabwriter.AlignRight)
	utw := tabwriter.NewWriter(&usb, 10, 8, 1, ' ', tabwriter.AlignRight)
	fmt.Fprintf(ltw, "resource/capa\tused(err)\twait-avg\t|   s m |\tserv-avg\t|   s m |\t\n")
	fmt.Fprintf(utw, "resource/capa\tused(err)\twait-avg\t|   s m |\tserv-avg\t|   s m |\t\n")
	for _, key := range semaNames {
		t := semaTraces[key]
		fmt.Fprintf(ltw, "%s\t%d(%d)\t%s\t%s\t%s\t%s\t\n", t.name, t.n, t.nerr, t.waitAvg.Round(time.Millisecond), histogram(t.waitBuckets), t.servAvg.Round(time.Millisecond), histogram(t.servBuckets))
		// bucket 5 = [1m,10m)
		// bucket 6 = [10m,*)
		if t.waitBuckets[5] > 0 || t.waitBuckets[6] > 0 || t.servBuckets[5] > 0 || t.servBuckets[6] > 0 {
			needToShow = true
			fmt.Fprintf(utw, "%s\t%d(%d)\t%s\t%s\t%s\t%s\t\n", t.name, t.n, t.nerr, ui.FormatDuration(t.waitAvg), histogram(t.waitBuckets), ui.FormatDuration(t.servAvg), histogram(t.servBuckets))
		}
	}
	ltw.Flush()
	utw.Flush()
	if needToShow {
		ui.Default.Infof("%s", usb.String())
	}
	return lsb.String()
}

var histchar = [...]string{"▂", "▃", "▄", "▅", "▆", "▇", "█"}

func histogram(b [7]int) string {
	max := 0
	for _, n := range b {
		if max < n {
			max = n
		}
	}
	var sb strings.Builder
	sb.WriteRune('|')
	for _, n := range b {
		if n <= 0 {
			sb.WriteRune(' ')
			continue
		}
		i := len(histchar) * n / (max + 1)
		sb.WriteString(histchar[i])
	}
	sb.WriteRune('|')
	return sb.String()
}

type semaTrace struct {
	name                     string
	n, nerr                  int
	waitAvg, servAvg         time.Duration
	waitBuckets, servBuckets [7]int
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

type dataSource struct {
	cache  cachestore.CacheStore
	client *reapi.Client
}

func (c *Command) initDataSource(ctx context.Context, credential cred.Cred) (dataSource, error) {
	layeredCache := build.NewLayeredCache()
	if c.localCacheEnable {
		cache, err := build.NewLocalCache(c.cacheDir)
		if err != nil {
			clog.Warningf(ctx, "failed to create local cache - no local cache enabled: %v", err)
		} else {
			layeredCache.AddLayer(cache)
			cache.GarbageCollectIfRequired(ctx)
		}
	} else {
		c.cacheDir = ""
	}
	var ds dataSource
	err := c.reopt.CheckValid()
	if err == nil {
		ds.client, err = reapi.New(ctx, credential, *c.reopt)
		if err != nil {
			return ds, err
		}
		layeredCache.AddLayer(ds.client.CacheStore())
	}
	ds.cache = layeredCache
	return ds, nil
}

func (ds dataSource) Close(ctx context.Context) error {
	if ds.client == nil {
		return nil
	}
	return ds.client.Close()
}

func (ds dataSource) DigestData(ctx context.Context, d digest.Digest, fname string) digest.Data {
	return digest.NewData(ds.Source(ctx, d, fname), d)
}

func (ds dataSource) Source(_ context.Context, d digest.Digest, fname string) digest.Source {
	return source{
		dataSource: ds,
		d:          d,
		fname:      fname,
	}
}

type source struct {
	dataSource dataSource
	d          digest.Digest
	fname      string
}

func (s source) Open(ctx context.Context) (io.ReadCloser, error) {
	var r io.ReadCloser
	var err error
	if s.dataSource.cache != nil {
		src := s.dataSource.cache.Source(ctx, s.d, s.fname)
		if src != nil {
			r, err = src.Open(ctx)
			if err == nil {
				return r, nil
			}
		}
		// fallback
	}
	if s.dataSource.client != nil {
		var buf []byte
		buf, err = s.dataSource.client.Get(ctx, s.d, s.fname)
		if err == nil {
			return io.NopCloser(bytes.NewReader(buf)), nil
		}
		// fallback
	}
	// ctx may be deadline exceeded or canceled.
	// if so, return such error.
	// DeadlineExceeded would trigger retry in hashfs flush.
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	// siso process runs at some directory, but
	// s.fname may not be relative to the working directory.
	// Actually, it is exec-root relative if it is created by
	// *Cmd.entriesFromResult, and failed to open as such path
	// doesn't exist. return with better error message.
	if !filepath.IsAbs(s.fname) {
		return nil, fmt.Errorf("failed to fetch source %v for %q: %w", s.d, s.fname, err)
	}
	// no reapi configured. use local file?
	f, err := os.Open(s.fname)
	return f, err
}

func (s source) String() string {
	return fmt.Sprintf("dataSource:%s", s.fname)
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

func (c *Command) initOutputLocal() (func(context.Context, string) bool, error) {
	switch c.outputLocalStrategy {
	case "full":
		return func(context.Context, string) bool { return true }, nil
	case "greedy":
		return func(ctx context.Context, fname string) bool {
			// Note: d. wil be downloaded to get deps anyway,
			// but will not be written to disk.
			switch filepath.Ext(fname) {
			case ".o", ".obj", ".a", ".d", ".stamp":
				return false
			}
			return true
		}, nil
	case "minimum":
		return func(ctx context.Context, fname string) bool {
			// force to output local for inputs
			// .h,/.hxx/.hpp/.inc/.c/.cc/.cxx/.cpp/.m/.mm for gcc deps or msvc showIncludes
			// .json/.js/.ts for tsconfig.json, .js for grit etc.
			// .py for protobuf py etc.
			switch filepath.Ext(fname) {
			case ".h", ".hxx", ".hpp", ".inc", ".c", ".cc", "cxx", ".cpp", ".m", ".mm", ".json", ".js", ".ts", ".py":
				return true
			}
			return false
		}, nil
	default:
		return nil, fmt.Errorf("unknown output local strategy: %q. should be full/greedy/minimum", c.outputLocalStrategy)
	}
}

type lastTargets struct {
	Targets []string `json:"targets,omitempty"`
	Failed  []string `json:"failed,omitempty"`
}

func loadTargets(targetsFile string) ([]string, []string, error) {
	buf, err := os.ReadFile(targetsFile)
	if err != nil {
		return nil, nil, err
	}
	var last lastTargets
	err = json.Unmarshal(buf, &last)
	if err != nil {
		return nil, nil, fmt.Errorf("parse error %s: %w", targetsFile, err)
	}
	return last.Targets, last.Failed, nil
}

func saveTargets(targetsFile string, targets, failed []string) error {
	v := lastTargets{
		Targets: targets,
		Failed:  failed,
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal last targets: %w", err)
	}
	err = os.WriteFile(targetsFile, buf, 0644)
	if err != nil {
		return fmt.Errorf("save last targets: %w", err)
	}
	return nil
}

func checkTargets(ctx context.Context, lastTargetsFilename string, targets []string) ([]string, bool) {
	lastTargets, failed, err := loadTargets(lastTargetsFilename)
	if err != nil {
		clog.Warningf(ctx, "checkTargets: %v", err)
		return nil, false
	}
	if len(targets) != len(lastTargets) {
		return nil, false
	}
	sort.Strings(targets)
	sort.Strings(lastTargets)
	for i := range targets {
		if targets[i] != lastTargets[i] {
			return nil, false
		}
	}
	return failed, true
}

func argsGN(args, key string) string {
	for line := range strings.SplitSeq(args, "\n") {
		i := strings.Index(line, "#")
		if i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, key) {
			continue
		}
		value := strings.TrimPrefix(line, key)
		value = strings.TrimSpace(value)
		if !strings.HasPrefix(value, "=") {
			continue
		}
		value = strings.TrimPrefix(value, "=")
		return strings.TrimSpace(value)
	}
	return ""
}

func cpuinfo() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "cpu family=%d model=%d stepping=%d ", cpuid.CPU.Family, cpuid.CPU.Model, cpuid.CPU.Stepping)
	fmt.Fprintf(&sb, "brand=%q vendor=%q ", cpuid.CPU.BrandName, cpuid.CPU.VendorString)
	fmt.Fprintf(&sb, "physicalCores=%d threadsPerCore=%d logicalCores=%d ", cpuid.CPU.PhysicalCores, cpuid.CPU.ThreadsPerCore, cpuid.CPU.LogicalCores)
	fmt.Fprintf(&sb, "vm=%t features=%s", cpuid.CPU.VM(), cpuid.CPU.FeatureSet())
	return sb.String()
}

func gcinfo() string {
	var sb strings.Builder
	memoryLimit := debug.SetMemoryLimit(-1) // not adjust the limit, but retrieve current limit
	if memoryLimit == math.MaxInt64 {
		// initial settings
		fmt.Fprintf(&sb, "memory_limit=unlimited ")
	} else {
		fmt.Fprintf(&sb, "memory_limit=%d (GOMEMLIMIT=%s) ", memoryLimit, os.Getenv("GOMEMLIMIT"))
	}

	gcPercent := debug.SetGCPercent(100) // 100 is default
	if gcPercent < 0 {
		ui.Default.PrintLines(ui.SGR(ui.BackgroundRed, fmt.Sprintf("Garbage collection is disabled. GOGC=%s\n", os.Getenv("GOGC"))))
		fmt.Fprintf(&sb, "gc=off")
	} else {
		fmt.Fprintf(&sb, "gc=%d", gcPercent)
	}
	debug.SetGCPercent(gcPercent) // restore original setting
	if v := os.Getenv("GOGC"); v != "" {
		fmt.Fprintf(&sb, " (GOGC=%s)", v)
	}
	return sb.String()
}

func (c *Command) invocation(ctx context.Context, buildID, projectID, execRoot string, properties resultstore.Properties) *rspb.Invocation {
	var username string
	currentUser, err := user.Current()
	if err != nil {
		clog.Warningf(ctx, "failed to get current user: %v", err)
		username = "unknownuser"
	} else {
		username = currentUser.Username
	}
	hostname, err := os.Hostname()
	if err != nil {
		clog.Warningf(ctx, "failed to get hostname: %v", err)
		hostname = "unknownhost"
	}

	return &rspb.Invocation{
		Timing: &rspb.Timing{
			StartTime: timestamppb.New(c.started),
		},
		InvocationAttributes: &rspb.InvocationAttributes{
			ProjectId:   projectID,
			Users:       []string{username},
			Labels:      []string{"siso", "build"},
			Description: fmt.Sprintf("Invocation ID %s", buildID),
		},
		WorkspaceInfo: &rspb.WorkspaceInfo{
			Hostname:         hostname,
			WorkingDirectory: execRoot,
			ToolTag:          "siso",
			CommandLines:     c.commandLines(),
		},
		Properties: properties,
	}
}

func (c *Command) commandLines() []*rspb.CommandLine {
	var cmdlines []*rspb.CommandLine
	cmdlines = append(cmdlines, &rspb.CommandLine{
		Label:   "original",
		Tool:    os.Args[0],
		Args:    os.Args[1:],
		Command: "ninja",
	})
	cmdline := &rspb.CommandLine{
		Label:   "canonical",
		Tool:    os.Args[0],
		Args:    []string{"ninja"},
		Command: "ninja",
	}
	c.Flags.VisitAll(func(f *flag.Flag) {
		cmdline.Args = append(cmdline.Args, fmt.Sprintf("-%s=%s", f.Name, f.Value.String()))
	})
	cmdline.Args = append(cmdline.Args, c.Flags.Args()...)
	cmdlines = append(cmdlines, cmdline)
	return cmdlines
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
