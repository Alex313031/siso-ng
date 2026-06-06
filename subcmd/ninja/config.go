// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	log "github.com/golang/glog"
	"github.com/google/uuid"
	"golang.org/x/term"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/buildconfig"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/sync/lockfile"
	"go.chromium.org/build/siso/toolsupport/watchmanutil"
	"go.chromium.org/build/siso/ui"
)

// batchFlag implements flag.Value and flag.BoolFlag for the -batch flag.
type batchFlag struct {
	c *Command
}

// IsBoolFlag returns true, indicating that batchFlag is a boolean flag.
func (*batchFlag) IsBoolFlag() bool { return true }

// String returns the string representation of the batch flag's current state.
func (f *batchFlag) String() string {
	if f == nil || f.c == nil {
		return "false"
	}
	// Accessing fields through the embedded struct
	return strconv.FormatBool(!f.c.fastNop && !f.c.fastLocal && !f.c.fastLastFailure && !f.c.fastExit)
}

// Set parses the string value `v` and sets the batch flag's state accordingly.
func (f *batchFlag) Set(v string) error {
	if f == nil || f.c == nil {
		return errors.New("nil batchFlag")
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return err
	}
	// Accessing fields through the embedded struct
	f.c.fastNop = !b
	f.c.fastLocal = !b
	f.c.fastLastFailure = !b
	f.c.fastExit = !b
	return nil
}

// NinjaFlags holds all configuration flags for the ninja command.
type NinjaFlags struct {
	outDir     ninjabuild.DirFlag
	configName string
	projectID  string

	buildID   string
	jobID     string
	namespace string

	offline         bool
	fastNop         bool
	fastLocal       bool
	fastLastFailure bool
	fastExit        bool

	quiet           bool
	heartbeatPeriod time.Duration

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

	cacheEnableRead bool

	configFilename string

	outputLocalStrategy string

	depsLogFile string

	stateDir string

	logDir             string
	frontendFile       string
	failureSummaryFile string
	failedCommandsFile string
	outputLogFile      string
	explainFile        string
	localexecLogFile   string
	invocationJSON     string
	metricsJSON        string
	traceJSON          string
	buildPprof         string

	fsopt              *hashfs.Option
	reopt              *reapi.Option
	reExecEnable       bool
	reCacheEnableRead  bool
	reCacheEnableWrite bool
	reproxyAddr        string

	artfsDir      string
	artfsEndpoint string

	cartfsEndpoint string

	enableCloudLogging          bool
	enableResultstore           bool
	enableCollector             bool
	collectorAddress            string
	enableCloudProfiler         bool
	cloudProfilerServiceName    string
	enableCloudTrace            bool
	enableCloudMonitoring       bool
	enableBuildNinjaFilesUpload bool
	metricsLabels               string
	metricsProject              string
	writeReclientMetricsLogs    bool
	traceThreshold              time.Duration
	traceSpanThreshold          time.Duration

	subtool    string
	cleandead  bool
	debugMode  debugMode
	adjustWarn string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	c.outDir.RegisterFlags(flagSet)
	flagSet.StringVar(&c.configName, "config", "", "config name passed to starlark")
	flagSet.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	defaultBuildID := os.Getenv("SISO_BUILD_ID")
	if defaultBuildID == "" {
		defaultBuildID = uuid.New().String()
	}
	flagSet.StringVar(&c.buildID, "build_id", defaultBuildID, "ID for the build. used for `invocation_id` of remote-apis-sdks and `tool_invocation_id` of remote-apis, and Cloud logging resource `build_id` label.")
	flagSet.StringVar(&c.jobID, "job_id", uuid.New().String(), "ID for a grouping of related builds such as a Buildbucket job. used for `correlated_invocations_id` of remote-apis and remote-apis-sdks, and Cloud logging resource `job_id` label.")
	flagSet.StringVar(&c.namespace, "namespace", "", "namespace for cloud logging's resource label")

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

	isTerminal := term.IsTerminal(int(os.Stdout.Fd()))

	flagSet.BoolVar(&c.fastNop, "fast_nop", isTerminal, "enable fast nop check")
	flagSet.BoolVar(&c.fastLocal, "fast_local", isTerminal, "enable fast local")
	flagSet.BoolVar(&c.fastLastFailure, "fast_last_failure", isTerminal, "enable fast last failure check")
	flagSet.BoolVar(&c.fastExit, "fast_exit", isTerminal, "enable fast exit")
	batch := &batchFlag{c: c}
	flagSet.Var(batch, "batch", "batch mode. prefer thoughput over low latency for build failures. disable -fast_nop, -fast_local -fast_last_failure -fast_exit")

	flagSet.BoolVar(&c.quiet, "quiet", false, "don't show progress status, just command output")
	flagSet.DurationVar(&c.heartbeatPeriod, "heartbeat_period", 0, "print a heartbeat with this frequency on the console when --quiet is set and this value is non zero.")
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

	c.setLocalCacheFlags(flagSet)
	flagSet.BoolVar(&c.cacheEnableRead, "cache_enable_read", true, "cache enable read")

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
	flagSet.StringVar(&c.outputLogFile, "output_log", "siso_output", "output log filename (relative to -log_dir)")
	flagSet.StringVar(&c.explainFile, "explain_log", "siso_explain", "explain log filename (relative to -log_dir)")
	flagSet.StringVar(&c.localexecLogFile, "localexec_log", "siso_localexec", "localexec log filename (relative to -log_dir)")
	flagSet.StringVar(&c.invocationJSON, "invocation_json", "siso_metadata.json", "invocation metadata JSON filename (relative to -log_dir)")
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

	// TODO(b/513044090): discover cartfs endpoint automatically?
	flagSet.StringVar(&c.cartfsEndpoint, "cartfs_endpoint", "", "cartfs server endpoint. e.g. localhost:65001")

	flagSet.DurationVar(&c.traceThreshold, "trace_threshold", 1*time.Minute, "threshold for trace record")
	flagSet.DurationVar(&c.traceSpanThreshold, "trace_span_threshold", 100*time.Millisecond, "theshold for trace span record")

	flagSet.BoolVar(&c.enableCollector, "enable_collector", false, "enable OTEL collector. Effectively not active, will be removed later. TODO: b/455433899.")
	flagSet.StringVar(&c.collectorAddress, "collector_address", os.Getenv("SISO_COLLECTOR_ADDRESS"), "address to dial the collector. Can be path for unix socket unix:///path/to/socket or host:port.")
	flagSet.BoolVar(&c.enableCloudLogging, "enable_cloud_logging", false, "enable cloud logging")
	flagSet.BoolVar(&c.enableResultstore, "enable_resultstore", false, "enable resultstore")
	flagSet.BoolVar(&c.enableCloudProfiler, "enable_cloud_profiler", false, "enable cloud profiler")
	flagSet.StringVar(&c.cloudProfilerServiceName, "cloud_profiler_service_name", "siso", "cloud profiler service name")
	flagSet.BoolVar(&c.enableCloudTrace, "enable_cloud_trace", false, "enable cloud trace")
	flagSet.BoolVar(&c.enableCloudMonitoring, "enable_cloud_monitoring", false, "enable cloud monitoring")
	flagSet.BoolVar(&c.enableBuildNinjaFilesUpload, "enable_build_ninja_files_upload", false, "enable Build Ninja files upload to RBE-CAS")
	flagSet.StringVar(&c.metricsLabels, "metrics_labels", os.Getenv("RBE_metrics_labels"), "comma-separated arbitrary key value pairs in the form key=value, which are added to cloud monitoring metrics and siso_metadata.json.")
	flagSet.StringVar(&c.metricsProject, "metrics_project", os.Getenv("RBE_metrics_project"), "override Cloud Monitoring GCP project where Siso sends action and build metrics.")
	flagSet.BoolVar(&c.writeReclientMetricsLogs, "write_reclient_metrics_logs", false, "write Reclient's RBE build metrics to rbe_metrics.{txt, pb} under -log_dir.")

	flagSet.StringVar(&c.subtool, "t", "", "run a subtool (use '-t list' to list subtools)")
	flagSet.BoolVar(&c.cleandead, "cleandead", false, "clean built files that are no longer produced by the manifest")
	flagSet.Var(&c.debugMode, "d", "enable debugging (use '-d list' to list modes)")
	flagSet.StringVar(&c.adjustWarn, "w", "", "adjust warnings. not supported b/288807840")
}

// initConfigFlags initializes a map of flag values to be used in starlark configuration.
func (c *Command) initConfigFlags(targets []string) map[string]string {
	flags := make(map[string]string)
	c.Flags.Visit(func(f *flag.Flag) {
		name := f.Name
		if name == "C" {
			name = "dir"
		}
		flags[name] = f.Value.String()
	})
	flags["project"] = c.projectID
	flags["reapi_address"] = c.reopt.Address
	flags["reapi_instance"] = c.reopt.Instance
	flags["is_terminal"] = strconv.FormatBool(term.IsTerminal(int(os.Stdout.Fd())))
	flags["is_smart_terminal"] = strconv.FormatBool(ui.IsTerminal())
	flags["targets"] = strings.Join(targets, " ")
	return flags
}

// initConfig initializes the build configuration by loading and parsing the main starlark file.
// If no Starlark config exists, it returns a default config that runs all steps locally.
// It also captures `args.gn` content if available.
func (c *Command) initConfig(ctx context.Context, workspaceRoot string, targets []string) (*buildconfig.Config, error) {
	defer trace.Begin(ctx, "initConfig").End()

	flags := c.initConfigFlags(targets)
	if c.configFilename == "" {
		return buildconfig.NewDefault(flags), nil
	}
	configRepoDir := filepath.Join(workspaceRoot, c.outDir.ConfigRepoDir)
	if _, err := os.Stat(configRepoDir); errors.Is(err, fs.ErrNotExist) {
		clog.Infof(ctx, "no config repo dir %s, using default config", configRepoDir)
		return buildconfig.NewDefault(flags), nil
	}
	cfgrepos := map[string]fs.FS{
		"config":           os.DirFS(configRepoDir),
		"config_overrides": os.DirFS(filepath.Join(workspaceRoot, ".siso_remote")),
	}
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

// changeToWorkdir establishes the execution root and working directory.
// It changes the current directory to working directory, detects the
// execution root, and updates path configurations to be relative to the root.
// It returns build path (workspace and ninja dir).
func (c *Command) changeToWorkdir(ctx context.Context) (*build.Path, error) {
	// The formatting of this string, complete with funny quotes, is
	// so Emacs can properly identify that the cwd has changed for
	// subsequent commands.
	// Don't print this if a tool is being used, so that tool output
	// can be piped into a file without this string showing up.
	if c.subtool == "" && c.outDir.Dir != "." {
		ui.Default.PrintLines(fmt.Sprintf("ninja: Entering directory `%s'\n\n", c.outDir.Dir))
	}
	startDir, workspaceRoot, dir, err := ninjabuild.InitDir(ctx, c.outDir)
	if err != nil {
		return nil, err
	}
	c.startDir = startDir
	clog.Infof(ctx, "working directory in workspace: %s", dir)
	if c.startDir != workspaceRoot {
		ui.Default.Printf("workspace=%s dir=%s\n", workspaceRoot, dir)
	}
	_, err = os.Stat(c.fname)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s not found in %s. need `-C <dir>`?", c.fname, filepath.Join(workspaceRoot, dir))
	}
	return build.NewPath(workspaceRoot, dir), err
}

// resolveFlags validates and adjusts flag values after they have been parsed.
// It checks for unsupported flags, sets defaults, and modifies flags
// based on the values of others.
func (c *Command) resolveFlags() error {
	err := c.debugMode.check()
	if err != nil {
		return flagError{err: err}
	}
	c.cleandead, err = checkSubtool(c.subtool)
	if err != nil {
		return err
	}

	if c.ninjaJobs >= 0 {
		ui.Default.Warningf("-j is deprecated. use -remote_jobs and/or -local_jobs instead\n")
	}
	if c.ninjaLoadLimit >= 0 {
		ui.Default.Warningf("-l is not supported.\n")
	}
	if c.failuresAllowed <= 0 {
		c.failuresAllowed = math.MaxInt
	}
	if c.adjustWarn != "" {
		ui.Default.Warningf("-w is specified. but not supported. b/288807840\n")
	}

	if err = uuid.Validate(c.buildID); err != nil {
		return flagError{err: fmt.Errorf("%q is an invalid build ID. -build_id must be a UUID", c.buildID)}
	}
	if len(c.jobID) > 1024 {
		return flagError{err: fmt.Errorf("-job_id length must be less than 1024")}
	}
	return nil
}

func (c *Command) enableOfflineMode(ctx context.Context) {
	if alex313031.IsNG() {
		ui.Default.Infof(ui.SGR(ui.Reset, "Offline mode\n"))
	} else {
		ui.Default.Warningf(ui.SGR(ui.Red, "offline mode\n"))
	}
	clog.Warningf(ctx, "offline mode")
	c.reopt = new(reapi.Option)
	c.reopt.Insecure = true
	c.projectID = ""
	c.collectorAddress = ""
	c.enableCloudLogging = false
	c.enableResultstore = false
	c.enableCloudProfiler = false
	c.enableCloudTrace = false
	c.enableCloudMonitoring = false
	c.reproxyAddr = ""
}

// initDepsLog loads the dependency log file (`.siso_deps`).
// It will recompact the log if necessary.
func initDepsLog(ctx context.Context, stateDir string, depsLogFile string) (*ninjabuild.DepsLog, error) {
	defer trace.Begin(ctx, "initDepsLog").End()

	depsLogPath := filepath.Join(stateDir, depsLogFile)
	err := os.MkdirAll(filepath.Dir(depsLogPath), 0755)
	if err != nil {
		clog.Warningf(ctx, "failed to mkdir for deps log: %v", err)
		return nil, err
	}
	depsLog, err := ninjabuild.NewDepsLog(ctx, depsLogPath)
	if err != nil {
		clog.Warningf(ctx, "failed to load deps log: %v", err)
		return nil, err
	}
	return depsLog, nil
}

// initBuildOpts initializes the `build.Options` struct by collecting
// various configuration settings and parameters.
func (c *Command) initBuildOpts(ctx context.Context, projectID string, buildPath *build.Path, config *buildconfig.Config, ds build.DataSource, hashFS *hashfs.HashFS, limits build.Limits, tracer *trace.Tracer, traceExporter *trace.Exporter, logWriters logWriters) build.Options {
	defer trace.Begin(ctx, "initBuildOpts").End()
	var actionSaltBytes []byte
	if c.actionSalt != "" {
		actionSaltBytes = []byte(c.actionSalt)
	}

	cache, err := build.NewCache(ctx, build.CacheOptions{
		Store:      ds.Cache,
		EnableRead: c.cacheEnableRead,
	})
	if err != nil {
		clog.Warningf(ctx, "no cache enabled: %v", err)
	}
	return build.Options{
		JobID:                 c.jobID,
		ID:                    c.buildID,
		StartTime:             c.started,
		ProjectID:             projectID,
		Metadata:              config.Metadata,
		Path:                  buildPath,
		HashFS:                hashFS,
		REAPIClient:           ds.Client,
		REExecEnable:          c.reExecEnable,
		RECacheEnableRead:     c.reCacheEnableRead,
		RECacheEnableWrite:    c.reCacheEnableWrite,
		ReproxyAddr:           c.reproxyAddr,
		ActionSalt:            actionSaltBytes,
		OutputLocal:           build.OutputLocalFunc(c.outputLocal),
		Cache:                 cache,
		FailureSummaryWriter:  logWriters.failureSummaryWriter,
		FailedCommandsWriter:  logWriters.failedCommandsWriter,
		OutputLogWriter:       logWriters.outputLogWriter,
		ExplainWriter:         logWriters.explainWriter,
		LocalexecLogWriter:    logWriters.localexecLogWriter,
		MetricsJSONWriter:     logWriters.metricsJSONWriter,
		TraceExporter:         traceExporter,
		Tracer:                tracer,
		Pprof:                 c.buildPprof,
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
}

func defaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		log.Warningf("Failed to get user cache dir: %v", err)
		return ""
	}
	return filepath.Join(d, "siso")
}

// initOutputLocal returns a function that determines whether a given file
// should be outputted locally based on the chosen strategy. This is used to
// control which files are downloaded from the remote cache.
func initOutputLocal(outputLocalStrategy string) (func(context.Context, string) bool, error) {
	switch outputLocalStrategy {
	case "full":
		return func(context.Context, string) bool { return true }, nil
	case "greedy":
		return func(ctx context.Context, fname string) bool {
			// Note: d. wil be downloaded to get deps anyway,
			// but will not be written to disk.
			switch filepath.Ext(fname) {
			case ".o", ".obj", ".a", ".d", ".stamp", ".pcm":
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
		return nil, fmt.Errorf("unknown output local strategy: %q. should be full/greedy/minimum", outputLocalStrategy)
	}
}

// checkSubtool validates the subtool name.
// It returns true if the subtool is 'cleandead'.
func checkSubtool(subtool string) (bool, error) {
	cleandead := false
	switch subtool {
	case "":
	case "list":
		return false, flagError{
			err: errors.New(`ninja subtools:
  commands   Use "siso query commands" instead
  deps       Use "siso query deps" instead
  inputs     Use "siso query inputs" instead
  targets    Use "siso query targets" instead
  cleandead  clean built files that are no longer produced by the manifest`),
		}
	case "commands":
		return false, flagError{
			err: errors.New("use `siso query commands` instead"),
		}
	case "deps":
		return false, flagError{
			err: errors.New("use `siso query deps` instead"),
		}
	case "inputs":
		return false, flagError{
			err: errors.New("use `siso query inputs` instead"),
		}
	case "targets":
		return false, flagError{
			err: errors.New("use `siso query targets` instead"),
		}

	case "cleandead":
		cleandead = true
	default:
		return false, flagError{err: fmt.Errorf("unknown tool %q", subtool)}
	}
	return cleandead, nil
}

// initLock creates and acquires a lock on the `.siso_lock` file in the state directory.
// It returns a function that will release the lock.
func initLock(ctx context.Context, dryRun bool, stateDir string) (func(), error) {
	if dryRun {
		return func() {}, nil
	}
	lockFilename := filepath.Join(stateDir, ".siso_lock")
	lock, err := lockfile.New(lockFilename)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		clog.Warningf(ctx, "lockfile is not supported")
		return func() {}, nil
	case err != nil:
		return nil, err
	default:
		var owner string
		spin := ui.Default.NewSpinner()
		for {
			err = lock.Lock()
			if alreadyLocked, ok := errors.AsType[*lockfile.ErrAlreadyLocked](err); ok {
				if owner != alreadyLocked.Owner {
					if owner != "" {
						spin.Done("lock holder %s completed", owner)
					}
					owner = alreadyLocked.Owner
					spin.Start("waiting for lock holder %s..", owner)
				}
				select {
				case <-ctx.Done():
					return nil, context.Cause(ctx)
				case <-time.After(500 * time.Millisecond):
					continue
				}
			} else if err != nil {
				spin.Stop(err)
				return nil, err
			}
			if owner != "" {
				spin.Done("lock holder %s completed", owner)
			}
			break
		}
		return func() {
			err := lock.Unlock()
			if err != nil {
				ui.Default.Errorf("failed to unlock %s: %v\n", lockFilename, err)
			}
			err = lock.Close()
			if err != nil {
				ui.Default.Errorf("failed to close %s: %v\n", lockFilename, err)
			}
		}, nil
	}
}

// initFSMonitor initializes a file system monitor like Watchman, based on
// the `SISO_FSMONITOR` environment variable.
// It returns an `fs.FSMonitor` implementation.
func initFSMonitor(ctx context.Context, workspaceRoot string) hashfs.FSMonitor {
	fsmonitor := os.Getenv("SISO_FSMONITOR")
	if fsmonitor == "" {
		return nil
	}
	var fsmonitorPath string
	var err error
	if !filepath.IsAbs(fsmonitor) {
		fsmonitorPath, err = exec.LookPath(fsmonitor)
		if err != nil {
			clog.Warningf(ctx, "failed to find fsmonitor %q: %v", fsmonitor, err)
			ui.Default.Warningf("%s", ui.SGR(ui.BackgroundRed, fmt.Sprintf("SISO_FSMONITOR=%q: failed %v\n", fsmonitor, err)))
			return nil
		}
	} else {
		fsmonitorPath = fsmonitor
	}
	fsm := strings.TrimSuffix(filepath.Base(fsmonitor), filepath.Ext(fsmonitor))
	switch fsm {
	case "watchman":
		wm, err := watchmanutil.New(ctx, fsmonitorPath, workspaceRoot)
		if err != nil {
			clog.Warningf(ctx, "failed to initialize watchman: %v", err)
			ui.Default.Errorf("%s", ui.SGR(ui.BackgroundRed, fmt.Sprintf("SISO_FSMONITOR=watchman: failed %v\n", err)))
			return nil
		}
		ui.Default.Infof("%s", ui.SGR(ui.Yellow, fmt.Sprintf("use watchman as fsmonitor: %s\n", fsmonitorPath)))
		return wm
	default:
		ui.Default.Errorf("%s", ui.SGR(ui.BackgroundRed, fmt.Sprintf("unknown SISO_FSMONITOR=%q (%q)\n", fsmonitor, fsm)))
	}
	return nil
}

type localCacheOptions struct {
	localCacheEnable bool
	cacheDir         string
}

func (c *Command) setLocalCacheFlags(flagSet *flag.FlagSet) {
	flagSet.BoolVar(&c.localCacheEnable, "local_cache_enable", false, "local cache enable")
	flagSet.StringVar(&c.cacheDir, "cache_dir", defaultCacheDir(), "cache directory")
}
