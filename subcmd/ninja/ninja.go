// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninja implements the subcommand `ninja` which parses a `build.ninja` file and builds the requested targets.
package ninja

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/google/subcommands"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/localexec"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/monitoring"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/signals"
	"go.chromium.org/build/siso/toolsupport/cartfsutil"
	"go.chromium.org/build/siso/toolsupport/cogutil"
	"go.chromium.org/build/siso/toolsupport/soongutil"
	"go.chromium.org/build/siso/ui"
)

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

	NinjaFlags
	localCacheOptions

	outputLocal func(context.Context, string) bool

	sisoInfoLog string // abs or relative to logDir
	startDir    string
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
		ui.Default = quietUI{
			heartbeatPeriod: c.heartbeatPeriod,
		}
		spin := ui.Default.NewSpinner()
		spin.Start("")
		defer spin.Stop(nil)
	} else if c.frontendFile != "" {
		f := os.Stdout
		if c.frontendFile != "-" {
			f, err = os.OpenFile(c.frontendFile, os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				ui.Default.Errorf("failed to open frontend file: %v\n", err)
				return subcommands.ExitFailure
			}
			defer func() {
				err = f.Close()
				if err != nil {
					ui.Default.Errorf("failed to close frontend file: %v\n", err)
				}
			}()
			// defer digest calculation for fast nop build on android
			c.fsopt.DeferDigest = true
		}
		frontend := soongutil.NewFrontend(ctx, f)
		ui.Default = frontend
		defer frontend.Close()
	}

	stats, err := c.Run(ctx)
	return c.postRun(ctx, stats, err)
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

func (c *Command) setup(ctx context.Context) (buildPath *build.Path, doneLock func(), resetCrashOutput func(), err error) {
	err = c.resolveFlags()
	if err != nil {
		return nil, nil, nil, err
	}
	if c.offline {
		c.enableOfflineMode(ctx)
	}

	buildPath, err = c.changeToWorkdir(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	if c.stateDir != "." && c.fsopt.StateFile != "" {
		c.fsopt.StateFile = filepath.Join(c.stateDir, c.fsopt.StateFile)
	}
	doneLock, err = initLock(ctx, c.dryRun, c.stateDir)
	if err != nil {
		return nil, nil, nil, err
	}

	err = c.initLogDir(ctx)
	if err != nil {
		doneLock()
		return nil, nil, nil, err
	}
	clog.Infof(ctx, "siso log dir=%s", c.logDir)
	if c.buildPprof != "" && !filepath.IsAbs(c.buildPprof) {
		c.buildPprof = filepath.Join(c.logDir, c.buildPprof)
	}
	c.rotateSisoResult(ctx)
	c.cleanupReclientMetrics(ctx)

	resetCrashOutput, err = c.setupCrashOutput(ctx)
	if err != nil {
		doneLock()
		return nil, nil, nil, err
	}
	return buildPath, doneLock, resetCrashOutput, nil
}

func (c *Command) computeLimits(ctx context.Context) build.Limits {
	// compute default limits based on fstype of work dir (e.g. artfs),
	// not of workspace.
	limits := build.DefaultLimits(ctx)
	if c.localJobs > 0 {
		limits.Local = c.localJobs
	}
	if c.remoteJobs > 0 {
		limits.Remote = c.remoteJobs
		limits.REWrap = c.remoteJobs
	}
	if !c.fastLocal && (limits.FastLocal != 0 || limits.StartLocal != 0) && (limits.Remote > 0 || limits.REWrap > 0) {
		needWarn := true
		c.Flags.Visit(func(f *flag.Flag) {
			if f.Name == "fast_local" {
				needWarn = false
			}
			if f.Name == "frontend_file" {
				needWarn = false
			}
		})
		if needWarn {
			var changes []string
			if limits.FastLocal != 0 {
				changes = append(changes, fmt.Sprintf("fastlocal=%d->0", limits.FastLocal))
			}
			if limits.StartLocal != 0 {
				changes = append(changes, fmt.Sprintf("startlocal=%d->0", limits.StartLocal))
			}
			ui.Default.Warningf("%s", ui.SGR(ui.Yellow, fmt.Sprintf("disable fast local for non-interactive: %s\n use `--fast_local` to enable fast local with non-interactive mode\n",
				strings.Join(changes, " "))))
		}
		clog.Infof(ctx, "disable fastlocal, startlocal")
		limits.FastLocal = 0
		limits.StartLocal = 0
	}
	c.checkResourceLimits(ctx, limits)
	return limits
}

func (c *Command) initCredentials(ctx context.Context) (cred.Cred, error) {
	needsCreds := !c.offline && (c.reopt.NeedCred() || c.enableCloudLogging || c.enableResultstore || c.enableCloudProfiler || c.enableCloudTrace || c.enableCloudMonitoring)
	if !needsCreds {
		return cred.Cred{}, nil
	}

	credential, err := cred.New(ctx, c.reopt.ServiceURI(), c.authOpts)
	if err != nil {
		return cred.Cred{}, err
	}
	return credential, nil
}

// Exposed for e2e testing. To be reevaluated.
func (c *Command) Run(ctx context.Context) (stats build.Stats, finalErr error) {
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
			wg.Go(cleanup)
		}
		wg.Wait()
		spin.Stop(nil)
	}()

	ctx, cancel := context.WithCancelCause(ctx)
	defer signals.HandleInterrupt(ctx, func() {
		cancel(errInterrupted{})
	})()

	buildPath, doneLock, resetCrashOutput, err := c.setup(ctx)
	if err != nil {
		return stats, err
	}
	defer doneLock()
	defer resetCrashOutput()

	// Start the spawn helper now, while siso's heap is still small so its launch
	// fork is cheap. Rotate its previous log first (the helper creates a fresh
	// one), keeping it in step with the other siso_* logs. No-op on platforms that
	// don't use the helper.
	spawnHelperLog := filepath.Join(c.logDir, "siso_spawn_helper")
	rotateFiles(ctx, spawnHelperLog)
	if err := localexec.StartHelper(ctx, spawnHelperLog); err != nil {
		return stats, err
	}

	limits := c.computeLimits(ctx)
	projectID := c.reopt.UpdateProjectID(c.projectID)

	credential, err := c.initCredentials(ctx)
	if err != nil {
		return stats, err
	}
	tracer, err := c.initTracer(ctx)
	if err != nil {
		return stats, err
	}
	defer tracer.Close(ctx)
	ctx = trace.TracerContext(ctx, tracer)

	if c.enableCloudLogging {
		spin := ui.Default.NewSpinner()
		spin.Start("init cloud logging")
		logCtx, loggerURL, done, err := c.initCloudLogging(ctx, projectID, c.namespace, credential)
		spin.Stop(err)
		if err != nil {
			// b/335295396 Compile step hitting write requests quota
			// rather than build fails, fallback to glog.
			ui.Default.Warningf("fallback to glog\n")
			c.enableCloudLogging = false
		} else {
			// use stderr for confirm no-op step. b/288534744
			ui.Default.Infof("%s\n", loggerURL)
			pCleanups = append(pCleanups, done)
			ctx = logCtx
		}
	}
	clog.Infof(ctx, "siso version %s", c.version)
	// logging is ready.
	properties := c.buildProperties(ctx, buildPath)
	// log build properties
	for _, p := range properties {
		clog.Infof(ctx, "%s: %q", p.Key, p.Value)
	}
	clog.Infof(ctx, "job id: %q", c.jobID)
	clog.Infof(ctx, "build id: %q", c.buildID)
	clog.Infof(ctx, "project id: %q", projectID)
	clog.Infof(ctx, "commandline %q", os.Args)
	clog.Infof(ctx, "is_terminal=%t fast_nop=%t fast_local=%t fast_last_failure=%t fast_exit=%t", ui.IsTerminal(), c.fastNop, c.fastLocal, c.fastLastFailure, c.fastExit)

	spin := ui.Default.NewSpinner()
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
	if c.enableCloudMonitoring {
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
			isErr := finalErr != nil && !errors.Is(finalErr, errNothingToDo)
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
	config, err := c.initConfig(ctx, buildPath.WorkspaceRoot, targets)
	if err != nil {
		return stats, err
	}
	tracer.SetMetadata(config.Metadata)

	logWriters, done, err := c.initLogWriters(ctx, buildPath)
	if err != nil {
		return stats, err
	}
	// It mutates finalErr, hence passing over pointer.
	defer done(&finalErr)

	// Assign c.outputLocal before launching the errgroup below.
	// initBuildOpts (loadNinjaFiles goroutine) and setupHashFS (main
	// goroutine) both read it; previously they raced through
	// c.fsopt.OutputLocal.
	c.outputLocal, err = initOutputLocal(c.outputLocalStrategy)
	if err != nil {
		return stats, err
	}

	var eg, reeg errgroup.Group
	var needHashFSRefresh bool
	var ninjaLogWriter io.Writer // ninjaLogWriter is used to pass the ninja log writer from CheckManifest to the main thread's bopts to avoid truncation.
	octx := ctx

	// When check manifest is done including regenerating manifest, it's ready to start loading Ninja files. This channel is used to start ninja loading
	checkManifestDone := make(chan error)
	go func() {
		ctx := trace.NewThread(octx, "checkManifest")
		// use temporary hashfs to check manifest and load manifest
		// files.
		tempHashFS, err := hashfs.New(ctx, hashfs.Option{})
		if err != nil {
			checkManifestDone <- err
			return
		}
		defer tempHashFS.Close(ctx)
		emptyDepsLog := &ninjabuild.DepsLog{}
		tempDS := build.DataSource{}
		bopts := c.initBuildOpts(ctx, projectID, buildPath, config, tempDS, tempHashFS, limits, tracer, nil, logWriters)
		needHashFSRefresh, err = ninjabuild.CheckManifest(ctx, c.fname, buildPath, config, tempHashFS, emptyDepsLog, &bopts)
		if err != nil {
			checkManifestDone <- err
			return
		}
		ninjaLogWriter = bopts.NinjaLogWriter
		clog.Infof(ctx, "check manifest done")
		checkManifestDone <- nil
	}()
	var localDepsLog *ninjabuild.DepsLog
	eg.Go(func() error {
		ctx := trace.NewThread(octx, "initDepsLog")
		depsLog, err := initDepsLog(ctx, c.stateDir, c.depsLogFile)
		if err != nil {
			return err
		}
		localDepsLog = depsLog
		return nil
	})

	var reapiClient *reapi.Client
	if err := c.reopt.CheckValid(); err == nil {
		ui.Default.Infof("use %s\n", c.reopt)
		ctx = reapi.NewContext(ctx, nil)
		reapiClient, err = reapi.New(ctx, credential, *c.reopt)
		if err != nil {
			return stats, err
		}
		reeg.Go(func() error {
			ctx := trace.NewThread(ctx, "reapi init")
			err := func() error {
				defer trace.Begin(ctx, "reapi cred.Wait").End()
				return credential.Wait()
			}()
			if err != nil {
				return fmt.Errorf("failed to initialize credentials: %w", err)
			}
			eg, ctx := errgroup.WithContext(ctx)
			eg.Go(func() error {
				ctx := trace.NewThread(ctx, "reapi.Init")
				return reapiClient.Init(ctx)
			})
			if c.reExecEnable {
				eg.Go(func() error {
					ctx := trace.NewThread(ctx, "reapi.CheckWritable")
					return reapiClient.CheckWritable(ctx)
				})
			}
			return eg.Wait()
		})
	} else {
		if c.strictRemote {
			return stats, flagError{err: fmt.Errorf("no reapi specified, but remote is requested as --strict_remote: %w", err)}
		}
		if c.remoteJobs > 0 {
			return stats, flagError{err: fmt.Errorf("no reapi specified, but remote is requested as --remote_jobs=%d: %w", c.remoteJobs, err)}
		}
	}
	if !c.localCacheEnable {
		c.cacheDir = ""
	}

	ds := build.NewDataSource(ctx, credential, c.localCacheEnable, c.cacheDir, reapiClient)
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			isCanceled := errors.Is(err, context.Canceled)
			if !isCanceled {
				if st, ok := status.FromError(err); ok && st.Code() == codes.Canceled {
					isCanceled = true
				}
			}
			if isCanceled {
				clog.Infof(ctx, "close datasource: %v", err)
			} else {
				clog.Warningf(ctx, "close datasource: %v", err)
			}
		}
	}()
	hashFS, closeHashFS, err := c.setupHashFS(ctx, buildPath, ds)
	if err != nil {
		return stats, err
	}
	defer func() { c.saveFailedTargetsAndCommand(ctx, finalErr, targets) }()
	defer func() { closeHashFS(targets, finalErr) }()
	hashFSErr := hashFS.LoadErr()
	if hashFSErr != nil {
		ui.Default.Errorf("%s", ui.SGR(ui.BackgroundRed, fmt.Sprintf("unable to do incremental build as fs state is corrupted: %v\n", hashFSErr)))
	}

	lastFailed := hasLastFailedTargets(c.stateDir)
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

	if c.enableResultstore {
		cleanup, err := c.setupResultStore(ctx, projectID, buildPath, properties, credential, hashFS)
		if err != nil {
			return stats, err
		}
		defer func() { cleanup(finalErr) }()
	}
	bopts := c.initBuildOpts(ctx, projectID, buildPath, config, ds, hashFS, limits, tracer, traceExporter, logWriters)

	err = <-checkManifestDone
	if err != nil {
		return stats, err
	}
	spin.Start("loading %s...", c.fname)
	nstate, err := ninjabuild.Load(ctx, c.fname, buildPath)
	if err != nil {
		return stats, err
	}
	err = eg.Wait()
	spin.Stop(err)
	if err != nil {
		return stats, err
	}
	// Reuse the ninja log writer initialized in CheckManifest to avoid overwriting the log.
	bopts.NinjaLogWriter = ninjaLogWriter
	if needHashFSRefresh {
		started := time.Now()
		err := hashFS.WaitReady(ctx)
		if err != nil {
			clog.Warningf(ctx, "hashfs error: %v", err)
			return stats, err
		}
		// to avoid unexpected reconcile mtime
		hashFS.Forget(ctx, buildPath.WorkspaceRoot, []string{buildPath.MaybeFromRelative(ctx, "build.ninja.stamp")})
		err = hashFS.Refresh(ctx)
		if err != nil {
			clog.Warningf(ctx, "%s modified. failed to refresh hashfs %s: %v", c.fname, time.Since(started), err)
			return stats, err
		}
		clog.Infof(ctx, "%s modified. refresh hashfs %s", c.fname, time.Since(started))
	}
	if localDepsLog != nil {
		defer localDepsLog.Close()
	}
	// TODO(b/286501388): init concurrently for .siso_config/.siso_filegroups, build.ninja.
	if c.fsopt.KeepTainted {
		tainted := hashFS.TaintedFiles()
		if len(tainted) == 0 {
			ui.Default.Warningf("%s", ui.SGR(ui.Yellow, "no tainted generated files:\n"))
		} else if len(tainted) < 5 {
			ui.Default.Warningf("%s", ui.SGR(ui.Yellow, fmt.Sprintf("keep %d tainted files:\n %s\n", len(tainted), strings.Join(tainted, "\n "))))
		} else {
			ui.Default.Warningf("%s", ui.SGR(ui.Yellow, fmt.Sprintf("keep %d tainted files:\n %s\n ...more\n", len(tainted), strings.Join(tainted, "\n "))))
		}
	}

	spin.Start("load siso config")
	stepConfig, err := ninjabuild.NewStepConfig(ctx, config, buildPath, c.fname, c.stateDir)
	if err != nil {
		spin.Stop(err)
		return stats, err
	}
	spin.Stop(nil)
	graph := ninjabuild.NewGraph(ctx, c.fname, nstate, config, buildPath, hashFS, stepConfig, localDepsLog)

	// Set last failure targets if necessary, and remove the last failed targets file unconditionally.
	if c.fastLastFailure && !c.clobber {
		lastFailedTargets := loadLastFailedTargets(ctx, c.stateDir, targets)
		if len(lastFailedTargets) > 0 {
			bopts.LastFailureTargets = lastFailedTargets
			ui.Default.PrintLines(fmt.Sprintf(ui.SGR(ui.Yellow, "Prioritizing last failed targets: %s\n"), lastFailedTargets))
		}
	}
	removeLastFailedTargets(ctx, c.stateDir)

	err = c.writeInvocationInfo(ctx, metricsLabels, targets)
	if err != nil {
		return stats, err
	}

	err = reeg.Wait()
	if err != nil {
		return stats, err
	}
	return ninjabuild.Run(ctx, graph, bopts, targets, ninjabuild.RunNinjaOpts{
		Cleandead:     c.cleandead,
		Subtool:       c.subtool,
		EnableStatusz: true,
	})
}

// postRun prints build result messages and returns exit status based on the build stats and the error from Run().
func (c *Command) postRun(ctx context.Context, stats build.Stats, runErr error) subcommands.ExitStatus {
	defer trace.Begin(ctx, "postRun").End()
	var result SisoResult
	defer func() {
		err := c.writeSisoResult(result)
		if err != nil {
			clog.Errorf(ctx, "write result error: %v", err)
			return
		}
	}()

	d := time.Since(c.started)
	if c.writeReclientMetricsLogs {
		if err := c.writeReclientMetrics(d, stats); err != nil {
			clog.Warningf(ctx, "failed to write RBE build metrics: %v", err)
		}
	}
	sps := float64(stats.Done-stats.Skipped) / d.Seconds()
	dur := ui.FormatDuration(d)
	if runErr != nil {
		if errors.Is(runErr, errNothingToDo) {
			msgPrefix := "Everything is up-to-date"
			if ui.IsTerminal() {
				msgPrefix = ui.SGR(ui.Green, msgPrefix)
			}
			fmt.Printf("%s Nothing to do.\n", msgPrefix)
			return subcommands.ExitSuccess
		}
		result.Code = int(subcommands.ExitFailure)
		if _, ok := errors.AsType[flagError](runErr); ok {
			ui.Default.Errorf("%v\n", runErr)
		} else if errBuild, ok := errors.AsType[ninjabuild.BuildError](runErr); ok {
			if errTarget, ok := errors.AsType[build.TargetError](errBuild.Err); ok {
				msgPrefix := "Schedule Failure"
				result.Message = fmt.Sprintf("%s: %v", msgPrefix, errTarget)
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
				return subcommands.ExitFailure
			}
			if errMissingSource, ok := errors.AsType[build.MissingSourceError](errBuild.Err); ok {
				msgPrefix := "Schedule Failure"
				result.Message = fmt.Sprintf("%s: %v", msgPrefix, errMissingSource)
				if ui.IsTerminal() {
					dur = ui.SGR(ui.Bold, dur)
					msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
				}
				ui.Default.Errorf("\n%6s %s: %v\n", dur, msgPrefix, errMissingSource)
				return subcommands.ExitFailure
			}
			if errTooManyFallback, ok := errors.AsType[build.TooManyFallbackError](errBuild.Err); ok {
				if _, ok := errors.AsType[execute.ExitError](errTooManyFallback.Err); !ok {
					msgPrefix := "Infra failure"
					result.InfraFailure = true
					result.Message = fmt.Sprintf("%s: %v", msgPrefix, errTooManyFallback)
					if ui.IsTerminal() {
						dur = ui.SGR(ui.Bold, dur)
						msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
					}
					ui.Default.Errorf("\n%6s %s: %v\n", dur, msgPrefix, errTooManyFallback)
					return subcommands.ExitFailure
				}
				// fallthrough if too many fallback with exit error.
			}
			msgPrefix := "Build Failure"
			result.Message = fmt.Sprintf("%s: %v", msgPrefix, runErr)
			if ui.IsTerminal() {
				dur = ui.SGR(ui.Bold, dur)
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			}
			ui.Default.Errorf("\n%6s %s: %d done, %d failed, %d remaining - %.02f/s\n %v\n", dur, msgPrefix, stats.Done-stats.Skipped, stats.Fail, stats.Total-stats.Done, sps, errBuild.Err)
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
		} else {
			msgPrefix := "Error"
			result.Message = fmt.Sprintf("%s: %v", msgPrefix, runErr)
			if ui.IsTerminal() {
				msgPrefix = ui.SGR(ui.BackgroundRed, msgPrefix)
			}
			if status.Code(runErr) == codes.Unavailable {
				ui.Default.Errorf("\n%6s %s: could not connect to backend. If you want to build offline, pass `-o` or `--offline`\n %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, runErr)
			} else {
				ui.Default.Errorf("\n%6s %s: %v\n", ui.FormatDuration(time.Since(c.started)), msgPrefix, runErr)
			}
		}
		return subcommands.ExitFailure
	}
	msgPrefix := "Build Succeeded"
	if ui.IsTerminal() {
		dur = ui.SGR(ui.Bold, dur)
		msgPrefix = ui.SGR(ui.Green, msgPrefix)
	}
	ui.Default.Infof("\n%6s %s: %d steps - %.02f/s\n", dur, msgPrefix, stats.Done-stats.Skipped, sps)
	return subcommands.ExitSuccess

}

func (c *Command) saveFailedTargetsAndCommand(ctx context.Context, err error, targets []string) {
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
		errBuild, ok := errors.AsType[ninjabuild.BuildError](err)
		if !ok {
			return
		}
		stepError, ok := errors.AsType[build.StepError](errBuild.Err)
		if !ok {
			rerr := os.Remove(c.logFilename(c.failedCommandsFile, ""))
			if rerr != nil {
				clog.Warningf(ctx, "failed to remove failed command file: %v", rerr)
			}
			return
		}
		// store failed targets only when build steps failed.
		// i.e., don't store with error like context canceled, etc.
		clog.Infof(ctx, "record failed targets: %q", stepError.Target)
		serr := saveLastFailedTargets(c.stateDir, targets, []string{stepError.Target})
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
}

func (c *Command) setupHashFS(ctx context.Context, buildPath *build.Path, ds build.DataSource) (*hashfs.HashFS, func([]string, error), error) {
	defer trace.Begin(ctx, "setupHashFS").End()
	c.fsopt.DataSource = ds
	c.fsopt.OutputLocal = c.outputLocal
	if c.logDir == "." || c.logDir == buildPath.AbsBase() {
		cwd := buildPath.AbsBase()
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
		// expect logDir is outside of workspace.
		clog.Infof(ctx, "ignore .ninja_log")
		ninjaLogFname := buildPath.AbsFromRelative(".ninja_log")
		c.fsopt.Ignore = func(ctx context.Context, fname string) bool {
			return fname == ninjaLogFname
		}
	}
	cogfs, err := cogutil.New(ctx, buildPath.WorkspaceRoot)
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		clog.Warningf(ctx, "unable to use cog? %v", err)
	}
	if cogfs != nil {
		ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("build in cog: %s\n", cogfs.Info())))
		c.fsopt.CogFS = cogfs
	}
	if c.cartfsEndpoint != "" {
		cartfs, err := cartfsutil.New(ctx, c.cartfsEndpoint)
		if err != nil {
			return nil, nil, err
		}
		ui.Default.PrintLines(ui.SGR(ui.Yellow, "build on cartfs\n"))
		c.fsopt.CartFS = cartfs
	}

	c.fsopt.FSMonitor = initFSMonitor(ctx, buildPath.WorkspaceRoot)

	hashFS, err := hashfs.New(ctx, *c.fsopt)
	if err != nil {
		return nil, nil, err
	}
	close := func(targets []string, err error) {
		if c.fsopt.CartFS != nil {
			cerr := c.fsopt.CartFS.Close()
			if cerr != nil {
				clog.Errorf(ctx, "close cartfs: %v", cerr)
			}
		}
		shouldSetTargets := !c.dryRun && c.subtool == "" && !c.prepare && err == nil
		hashFS.SetBuildTargets(ctx, targets, shouldSetTargets)
		cerr := hashFS.Close(ctx)
		if cerr != nil {
			clog.Errorf(ctx, "close hashfs: %v", cerr)
		}
	}
	return hashFS, close, nil
}
