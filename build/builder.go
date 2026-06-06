// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/logging"
	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build/metadata"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/localexec"
	"go.chromium.org/build/siso/execute/remoteexec"
	"go.chromium.org/build/siso/execute/reproxyexec"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/hashfs/osfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/o11y/monitoring"
	sisopprof "go.chromium.org/build/siso/o11y/pprof"
	"go.chromium.org/build/siso/o11y/resultstore"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/scandeps"
	"go.chromium.org/build/siso/sync/semaphore"
	"go.chromium.org/build/siso/toolsupport/gccutil"
	"go.chromium.org/build/siso/toolsupport/makeutil"
	"go.chromium.org/build/siso/toolsupport/msvcutil"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
	"go.chromium.org/build/siso/ui"
)

// logging labels's key.
const (
	logLabelKeyID         = "id"
	logLabelKeyBacktraces = "backtraces"
)

// chromium recipe module expects this string.
const ninjaNoWorkToDo = "ninja: no work to do.\n"

// OutputLocalFunc is a function to determine the file should be downloaded or not.
type OutputLocalFunc func(context.Context, string) bool

// Options is builder options.
type Options struct {
	JobID     string
	ID        string
	StartTime time.Time

	Metadata           metadata.Metadata
	ProjectID          string
	Path               *Path
	HashFS             *hashfs.HashFS
	REAPIClient        *reapi.Client
	REExecEnable       bool
	RECacheEnableRead  bool
	RECacheEnableWrite bool
	ReproxyAddr        string
	ActionSalt         []byte

	OutputLocal          OutputLocalFunc
	Cache                *Cache
	NinjaLogWriter       io.Writer
	FailureSummaryWriter io.Writer
	FailedCommandsWriter io.Writer
	OutputLogWriter      io.Writer
	ExplainWriter        io.Writer
	LocalexecLogWriter   io.Writer
	MetricsJSONWriter    io.Writer
	Pprof                string
	Tracer               *trace.Tracer
	TraceExporter        *trace.Exporter
	PprofUploader        *sisopprof.Uploader
	ResultstoreUploader  *resultstore.Uploader

	// Clobber forces to rebuild ignoring existing generated files.
	Clobber bool

	// don't check slow termination.
	FastExit bool

	// Build inputs of targets, but not build targets itself.
	Prepare bool

	// Verbose shows all command lines while building rather than step description.
	Verbose bool

	// VerboseFailures shows failed command lines.
	VerboseFailures bool

	// DryRun just prints the command to build, but does nothing.
	DryRun bool

	// don't use local for remote steps.
	StrictRemote bool

	// allow failures at most FailuresAllowed.
	FailuresAllowed int

	// don't delete @response files on success
	KeepRSP bool

	// don't delete depfile.
	KeepDepfile bool

	// RebuildManifest is a build manifest filename (i.e. build.ninja)
	// when rebuilding manifest.
	// empty for normal build.
	RebuildManifest string

	// Limits specifies resource limits.
	Limits Limits

	// Upload Build Ninja files over REAPI
	UploadBuildNinjaFiles bool

	// LastFailureTargets is a list of targets that failed in the previous build.
	LastFailureTargets []string
}

// Builder is a builder.
type Builder struct {
	jobID string // correlated invocations id.
	// build session id, tool invocation id.
	id        string
	projectID string
	metadata  metadata.Metadata

	statusReporter StatusReporter
	progress       progress

	// path system used in the build.
	path   *Path
	hashFS *hashfs.HashFS

	// arg table to intern command line args of steps.
	argTab symtab

	// start is the time at the build starts.
	start time.Time
	graph Graph
	plan  *plan
	stats *stats

	// record target's state.
	targets sync.Map

	stepSema *semaphore.Semaphore

	preprocSema *semaphore.Prioritized

	// for subtree: dir -> *subtree
	trees sync.Map

	scanDepsSema *semaphore.Prioritized
	scanDeps     *scandeps.ScanDeps

	localSema *semaphore.Prioritized
	poolSemas map[string]*semaphore.Prioritized
	localExec localexec.LocalExec

	rewrapSema *semaphore.Prioritized

	fastLocalSema     *semaphore.Semaphore
	startLocalCounter atomic.Int32
	racingEnabled     bool

	remoteSema         *semaphore.Prioritized
	remoteExec         *remoteexec.RemoteExec
	reExecEnable       bool
	reCacheEnableRead  bool
	reCacheEnableWrite bool
	reapiclient        *reapi.Client

	reStatMu                  sync.Mutex
	reSchedStat, reWorkerStat semaphore.Stat

	reproxySema *semaphore.Prioritized
	reproxyExec *reproxyexec.REProxyExec

	actionSalt []byte

	outputLocal OutputLocalFunc

	cacheSema *semaphore.Semaphore
	cache     *Cache

	explainWriter        io.Writer
	ninjaLogWriter       io.Writer
	failureSummaryWriter io.Writer
	failedCommandsWriter io.Writer
	localexecLogWriter   io.Writer
	metricsJSONWriter    io.Writer
	outputLogWriter      io.Writer
	traceExporter        *trace.Exporter
	tracer               *trace.Tracer
	traceStats           *traceStats
	tracePprof           *tracePprof
	pprofUploader        *sisopprof.Uploader
	resultstoreUploader  *resultstore.Uploader

	tracePidPreproc, tracePidLocal, tracePidRemote, tracePidWorker int64

	// envfiles: filename -> *envfile
	envFiles sync.Map

	clobber bool

	fastExit bool

	prepare               bool
	verbose               bool
	verboseFailures       bool
	dryRun                bool
	strictRemote          bool
	UploadBuildNinjaFiles bool

	failures failures

	numFallback        atomic.Int64
	maxFallbackAllowed int64

	// ninja debug modes
	keepRSP     bool
	keepDepfile bool

	rebuildManifest string

	lastFailureTargets map[string]struct{}
}

// New creates new builder.
func New(ctx context.Context, graph Graph, opts Options) (_ *Builder, err error) {
	logger := clog.FromContext(ctx)
	if logger != nil {
		logger.Formatter = logFormat
	}
	start := opts.StartTime
	if start.IsZero() {
		start = time.Now()
	}
	var statusReporter StatusReporter = noopStatusReporter{}
	if sr, ok := ui.Default.(StatusReporter); ok {
		statusReporter = sr
	}
	ew := opts.ExplainWriter
	if ew == nil {
		ew = io.Discard
	}
	nw := opts.NinjaLogWriter
	if nw == nil {
		nw = io.Discard
	}
	lelw := opts.LocalexecLogWriter
	if lelw == nil {
		lelw = io.Discard
	}
	mw := opts.MetricsJSONWriter
	if mw == nil {
		mw = io.Discard
	}
	if err := opts.Path.Check(); err != nil {
		return nil, err
	}
	if opts.HashFS == nil {
		return nil, fmt.Errorf("hash fs must be set")
	}
	var le localexec.LocalExec
	var re *remoteexec.RemoteExec
	var pe *reproxyexec.REProxyExec
	if opts.REAPIClient != nil {
		if opts.RECacheEnableWrite && !opts.REAPIClient.UpdateActionResultEnabled() {
			return nil, fmt.Errorf("reapi doesn't support UpdateActionResult, required for --re_cache_enable_write")
		}

		logger.Infof("enable built-in remote exec")
		re = remoteexec.New(ctx, opts.REAPIClient)

	} else {
		logger.Infof("disable built-in remote exec")
	}
	pe = reproxyexec.New(ctx, opts.ReproxyAddr)
	if pe.Enabled() {
		logger.Infof("enable reclient integration: addr=%s", opts.ReproxyAddr)
	} else {
		logger.Infof("disable reclient integration")
	}
	experiments.ShowOnce()
	numCPU := runtime.GOMAXPROCS(0)
	if (opts.Limits == Limits{}) {
		opts.Limits = DefaultLimits(ctx)
	}
	// TODO(b/503546538): make it 0 by default.
	maxFallbackAllowed := int64(math.MaxInt64)
	switch {
	case opts.StrictRemote:
		logger.Infof("strict remote.  no fastlocal, no local fallback")
		opts.Limits.FastLocal = 0
		maxFallbackAllowed = 0
	case experiments.Enabled("allow-fallback-high", ""):
		maxFallbackAllowed = int64(math.MaxInt64)
	case experiments.Enabled("allow-fallback-low", ""):
		maxFallbackAllowed = 4
	case experiments.Enabled("allow-fallback-unlimited", ""):
		maxFallbackAllowed = int64(math.MaxInt64)
	case experiments.Enabled("no-fallback", ""):
		maxFallbackAllowed = 0
	}
	// On many cores machine, it would hit default max thread limit = 10000.
	// Usually, it would require 1/3 of stepLimit threads (cache miss case?).
	// For safe, sets 1/2 of stepLimit for max threads. b/325565625
	maxThreads := opts.Limits.Step / 2
	if maxThreads > 10000 {
		debug.SetMaxThreads(maxThreads)
	} else {
		maxThreads = 10000
	}
	logger.Infof("numcpu=%d threads:%d - limits=%#v", numCPU, maxThreads, opts.Limits)
	logger.Infof("correlated_invocations_id: %s", opts.JobID)
	logger.Infof("tool_invocation_id: %s", opts.ID)

	var fastLocalSema *semaphore.Semaphore
	if opts.Limits.FastLocal > 0 {
		fastLocalSema = semaphore.New("fastlocal", opts.Limits.FastLocal)
	}
	b := &Builder{
		jobID:     opts.JobID,
		id:        opts.ID,
		projectID: opts.ProjectID,
		metadata:  opts.Metadata,

		statusReporter: statusReporter,

		path:         opts.Path,
		hashFS:       opts.HashFS,
		start:        start,
		graph:        graph,
		stepSema:     semaphore.New("step", opts.Limits.Step),
		preprocSema:  semaphore.NewPrioritized("preproc", opts.Limits.Preproc),
		scanDepsSema: semaphore.NewPrioritized("scandeps", opts.Limits.ScanDeps),
		scanDeps: scandeps.New(ctx, opts.HashFS, scandeps.Options{
			InputDeps:                    graph.InputDeps(ctx),
			InputsRequiringClangScandeps: graph.InputsRequiringClangScandeps(ctx),
			ClangMode:                    graph.ClangScandeps(ctx),
		}),
		localSema:          semaphore.NewPrioritized("localexec", opts.Limits.Local),
		localExec:          le,
		rewrapSema:         semaphore.NewPrioritized("rewrap", opts.Limits.REWrap),
		fastLocalSema:      fastLocalSema,
		racingEnabled:      experiments.Enabled("racing", "racing mode enabled"),
		remoteSema:         semaphore.NewPrioritized("remoteexec", opts.Limits.Remote),
		remoteExec:         re,
		reExecEnable:       opts.REExecEnable,
		reCacheEnableRead:  opts.RECacheEnableRead || experiments.Enabled("simulate-remote-cache-misses", "simulate cache miss"),
		reCacheEnableWrite: opts.RECacheEnableWrite,
		reproxyExec:        pe,
		reproxySema:        semaphore.NewPrioritized("reproxyexec", opts.Limits.Remote),
		actionSalt:         opts.ActionSalt,
		reapiclient:        opts.REAPIClient,
		reSchedStat:        semaphore.Stat{Name: "re:sched"},
		reWorkerStat:       semaphore.Stat{Name: "re:worker"},

		outputLocal:           opts.OutputLocal,
		cacheSema:             semaphore.New("cache", opts.Limits.Cache),
		cache:                 opts.Cache,
		failureSummaryWriter:  opts.FailureSummaryWriter,
		failedCommandsWriter:  opts.FailedCommandsWriter,
		outputLogWriter:       opts.OutputLogWriter,
		explainWriter:         ew,
		ninjaLogWriter:        nw,
		localexecLogWriter:    lelw,
		metricsJSONWriter:     mw,
		traceExporter:         opts.TraceExporter,
		tracer:                opts.Tracer,
		traceStats:            newTraceStats(),
		tracePprof:            newTracePprof(opts.Pprof),
		pprofUploader:         opts.PprofUploader,
		resultstoreUploader:   opts.ResultstoreUploader,
		clobber:               opts.Clobber,
		fastExit:              opts.FastExit,
		prepare:               opts.Prepare,
		verbose:               opts.Verbose,
		verboseFailures:       opts.VerboseFailures,
		dryRun:                opts.DryRun,
		strictRemote:          opts.StrictRemote,
		failures:              failures{allowed: opts.FailuresAllowed},
		maxFallbackAllowed:    maxFallbackAllowed,
		keepRSP:               opts.KeepRSP,
		keepDepfile:           opts.KeepDepfile,
		rebuildManifest:       opts.RebuildManifest,
		UploadBuildNinjaFiles: opts.UploadBuildNinjaFiles,
		lastFailureTargets:    make(map[string]struct{}),
	}
	for _, t := range opts.LastFailureTargets {
		b.lastFailureTargets[t] = struct{}{}
	}
	if opts.Limits.StartLocal > 0 {
		b.startLocalCounter.Store(int32(opts.Limits.StartLocal))
	}
	if b.reapiclient != nil {
		reapiVer := b.reapiclient.APIVersion()
		clog.Infof(ctx, "reapi version=%v; output_paths=%t action.platform=%t", reapiVer, reapi.UseOutputPaths(reapiVer), reapi.UseActionForPlatformProperties(reapiVer))
	}
	if experiments.Enabled("ignore-missing-out-in-depfile", "ignore missing out error in depfile") {
		makeutil.IgnoreMissingOut = true
	}
	return b, nil
}

// Close cleans up the builder.
func (b *Builder) Close() error {
	if b.reproxyExec == nil {
		return nil
	}
	return b.reproxyExec.Close()
}

// Stats returns stats of the builder.
func (b *Builder) Stats() Stats {
	return b.stats.stats()
}

// TraceStats returns trace stats of the builder.
func (b *Builder) TraceStats() []*TraceStat {
	return b.traceStats.get()
}

// SemaStats returns semaphore stats of the builder.
func (b *Builder) SemaStats() []semaphore.Stat {
	stats := []semaphore.Stat{
		b.cache.sema.Stat(),
		b.cacheSema.Stat(),
		scandeps.CPPScanSema.Stat(),
		b.fastLocalSema.Stat(),
		hashfs.DigestSemaphore.Stat(),
		localexec.ForkSema.Stat(),
		hashfs.FlushSemaphore.Stat(),
		b.localSema.Stat(),
		osfs.LstatSemaphore.Stat(),
	}
	for _, name := range slices.Sorted(maps.Keys(b.poolSemas)) {
		stats = append(stats, b.poolSemas[name].Stat())
	}
	stats = append(stats,
		b.preprocSema.Stat(),
		b.reSchedStat,
		b.reWorkerStat,
		reapi.FileSemaphore.Stat(),
		remoteexec.Semaphore.Stat(), // remoteexec-digest
		b.rewrapSema.Stat(),
		b.remoteSema.Stat(),
		b.reproxySema.Stat(),
		b.scanDepsSema.Stat(),
	)
	return slices.DeleteFunc(stats, func(s semaphore.Stat) bool {
		return s.Name == "" || s.N == 0
	})
}

// ErrManifest is an error to indicate manifest error.
var ErrManifest = errors.New("manifest error")

// ErrManifestModified is an error to indicate that manifest is modified.
var ErrManifestModified = errors.New("manifest modified")

type numBytes int64

var bytesUnit = map[int64]string{
	1 << 10: "KiB",
	1 << 20: "MiB",
	1 << 30: "GiB",
	1 << 40: "TiB",
}

func (b numBytes) String() string {
	var n []int64
	for k := range bytesUnit {
		n = append(n, k)
	}
	sort.Slice(n, func(i, j int) bool {
		return n[i] > n[j]
	})
	i := int64(b)
	for _, k := range n {
		if i >= k {
			return fmt.Sprintf("%.02f%s", float64(i)/float64(k), bytesUnit[k])
		}
	}
	return fmt.Sprintf("%dB", i)
}

// Build builds args with the name.
func (b *Builder) Build(ctx context.Context, name string, args ...string) (err error) {
	started := time.Now()
	b.statusReporter.BuildStarted()
	defer b.statusReporter.BuildFinished()

	// pctx is parent context, that is used to check
	// original context is canceled or not.
	pctx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		if cerr := context.Cause(pctx); err == nil && cerr != nil {
			err = cerr
		}
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			loc := panicLocation(buf)
			clog.Errorf(ctx, "panic in build: %v\n%s", r, loc)
			if err == nil {
				err = fmt.Errorf("panic in build: %v", r)
			}
		}
		clog.Infof(ctx, "build %v", err)
	}()

	if b.rebuildManifest == "" {
		// record build files in hashfs. b/489164002
		for _, fname := range b.graph.Filenames() {
			_, err = b.hashFS.Stat(ctx, b.path.AbsBase(), fname)
			if err != nil {
				clog.Warningf(ctx, "failed to stat build file %q: %v", fname, err)
			}
		}
	}

	if b.UploadBuildNinjaFiles && b.rebuildManifest == "" && b.reapiclient != nil {
		// upload build.ninja in background.
		// if build finished earilier, we'll cancel the uploading
		// since it would be better to finish build soon rather
		// than waiting for uploading build.ninja.
		go b.uploadBuildNinja(ctx)
	}

	// scheduling
	// TODO: run asynchronously?
	knownTargetWeights := make(map[Target]int)
	if len(b.lastFailureTargets) > 0 {
		targets, err := b.graph.Targets(ctx, slices.Collect(maps.Keys(b.lastFailureTargets))...)
		if err != nil {
			clog.Warningf(ctx, "failed to get targets for known weights: %v", err)
		}
		for _, t := range targets {
			knownTargetWeights[t] = math.MaxInt
		}
	}
	schedOpts := schedulerOption{
		NumTargets:   b.graph.NumTargets(),
		Path:         b.path,
		HashFS:       b.hashFS,
		Prepare:      b.prepare,
		KnownWeights: knownTargetWeights,
	}
	sched := newScheduler(ctx, schedOpts)
	err = schedule(ctx, sched, b.graph, args...)
	if err != nil {
		if b.failureSummaryWriter != nil {
			fmt.Fprintf(b.failureSummaryWriter, "%v\n", err)
		}
		return err
	}
	b.plan = sched.plan
	b.stats = newStats(sched.total)
	b.statusReporter.PlanHasTotalSteps(sched.total)

	stat := b.Stats()
	if stat.Total == 0 {
		clog.Infof(ctx, "nothing to build for %q", args)
		fmt.Printf("\r%s", ninjaNoWorkToDo)
		return nil
	}

	var pools []string
	localLimit := b.localSema.Capacity()
	stepLimits := b.graph.StepLimits(ctx)
	for k := range stepLimits {
		pools = append(pools, k)
	}
	sort.Strings(pools)
	b.poolSemas = make(map[string]*semaphore.Prioritized)
	for _, k := range pools {
		v := stepLimits[k]
		name := "pool=" + k
		if strings.HasSuffix(name, "_pool") {
			// short name for semaphore/resource name
			// e.g. build_toolchain_action_pool -> pool=action
			s := strings.Split(name, "_")
			name = "pool=" + s[len(s)-2]
		}
		// No matter what pools you specify, ninja will never run more concurrent jobs than the default parallelism,
		if v > localLimit {
			v = localLimit
		}
		b.poolSemas[k] = semaphore.NewPrioritized(name, v)
		clog.Infof(ctx, "limit %s -> %s=%d", k, name, v)
	}

	var mftime time.Time
	if b.rebuildManifest != "" {
		fi, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, b.path.MaybeFromRelative(ctx, b.rebuildManifest))
		if err == nil {
			mftime = fi.ModTime()
			clog.Infof(ctx, "manifest %s: %s", b.rebuildManifest, mftime)
		}
	}
	defer func() {
		stat = b.Stats()
		if b.rebuildManifest != "" {
			fi, mferr := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, b.path.MaybeFromRelative(ctx, b.rebuildManifest))
			if mferr != nil {
				clog.Warningf(ctx, "failed to stat %s: %v", b.rebuildManifest, mferr)
				err = fmt.Errorf("%w: missing manifest %s: %v", ErrManifest, b.rebuildManifest, mferr)
				return
			}
			if err != nil {
				err = fmt.Errorf("%w: %v", ErrManifest, err)
				return
			}
			clog.Infof(ctx, "rebuild manifest %#v %s: %s->%s: %s", stat, b.rebuildManifest, mftime, fi.ModTime(), time.Since(started))
			if fi.ModTime().After(mftime) || stat.Done != stat.Skipped {
				ui.Default.PrintLines(fmt.Sprintf("%6s Regenerating ninja files\n\n", ui.FormatDuration(time.Since(started))))
				err = ErrManifestModified
				return
			}
			return
		}
		clog.Infof(ctx, "build %s %s: %v", time.Since(started), time.Since(b.start), err)
		if stat.Skipped == stat.Total {
			fmt.Printf("\r%s", ninjaNoWorkToDo)
			return
		}
		fsstat := b.hashFS.OS.Stats()
		fsstatLine := fmt.Sprintf("fs: ops: %d(err:%d) / r:%d(err:%d) %s / w:%d(err:%d) %s\n",
			fsstat.Ops, fsstat.OpsErrs,
			fsstat.ROps, fsstat.RErrs, numBytes(fsstat.RBytes),
			fsstat.WOps, fsstat.WErrs, numBytes(fsstat.WBytes))
		var depsStatLine string
		var restatLine string
		if b.reapiclient != nil {
			// scandeps is only used in siso native mode.
			if stat.ScanDepsFailed != 0 || stat.ClangScanDeps != 0 {
				depsStatLine = fmt.Sprintf("scanErr:%d cc-M:%d\n", stat.ScanDepsFailed, stat.ClangScanDeps)
			}
			restat := b.reapiclient.IOMetrics().Stats()
			restatLine = fmt.Sprintf("reapi: ops: %d(err:%d) / r:%d(err:%d) %s / w:%d(err:%d) %s\n",
				restat.Ops, restat.OpsErrs,
				restat.ROps, restat.RErrs, numBytes(restat.RBytes),
				restat.WOps, restat.WErrs, numBytes(restat.WBytes))
		}
		if !b.reproxyExec.Used() {
			// this stats will be shown by reproxy shutdown.
			msg := fmt.Sprintf("\nlocal:%d remote:%d cache:%d cache-write:%d(err:%d) fallback:%d retry:%d skip:%d\n",
				stat.Local+stat.NoExec, stat.Remote, stat.CacheHit, stat.CacheWrite, stat.CacheWriteErr, stat.LocalFallback, stat.RemoteRetry, stat.Skipped) +
				depsStatLine +
				restatLine +
				fsstatLine + "\n"
			ui.Default.PrintLines("\n", msg)
			if b.resultstoreUploader != nil {
				b.resultstoreUploader.AddBuildLog(msg + "\n")
			}
		} else {
			ui.Default.PrintLines("\n", "\n")
		}
	}()
	semas := []trace.Semaphore{
		b.cache.sema,
		b.localSema,
		b.remoteSema,
		b.reproxySema,
		b.rewrapSema,
		b.stepSema,
		hashfs.FlushSemaphore,
		hashfs.ForgetMissingsSemaphore,
		osfs.LstatSemaphore,
		localCacheSemaphore,
		reapi.FileSemaphore,
		gccutil.Semaphore,
		msvcutil.Semaphore,
		remoteexec.Semaphore,
	}
	b.tracer.Start(ctx, semas, []*iometrics.IOMetrics{
		b.hashFS.OS.IOMetrics,
		b.reapiclient.IOMetrics(),
		// TODO: cache iometrics?
	})
	defer b.tracer.Stop()
	b.tracePidPreproc = b.tracer.Process(ctx, "preproc")
	b.tracePidLocal = b.tracer.Process(ctx, "local-exec")
	b.tracePidRemote = b.tracer.Process(ctx, "remote-exec")
	b.tracePidWorker = b.tracer.Process(ctx, "rbe")
	b.tracePprof.SetMetadata(b.metadata)
	b.pprofUploader.SetMetadata(ctx, b.metadata)
	defer func(ctx context.Context) {
		perr := b.tracePprof.Close(ctx)
		if perr != nil {
			clog.Warningf(ctx, "pprof close: %v", perr)
		}
		if b.pprofUploader != nil {
			perr := b.pprofUploader.Upload(ctx, b.tracePprof.p)
			if perr != nil {
				clog.Warningf(ctx, "upload pprof: %v", perr)
			} else {
				clog.Infof(ctx, "uploaded pprof")
			}
		} else {
			clog.Infof(ctx, "no pprof uploader")
		}
	}(ctx)
	pstat := b.plan.stats()
	clog.Infof(ctx, "build pendings=%d ready=%d", pstat.npendings, pstat.nready)
	b.progress.report("build start: Ready %d Pending %d", pstat.nready, pstat.npendings)
	b.progress.start(ctx, b)
	defer b.progress.stop()

	if b.clobber {
		fmt.Fprintf(b.explainWriter, "--clobber is specified\n")
	}

	var wg sync.WaitGroup
	var stuck bool
	errch := make(chan error, 1000)

loop:
	for {
		t := time.Now()
		ctx, done, err := b.stepSema.WaitAcquire(ctx)
		if err != nil {
			clog.Warningf(ctx, "wait acquire: %v", err)
			cancel()
			return err
		}
		dur := time.Since(t)
		if dur > 1*time.Millisecond {
			clog.Infof(ctx, "step sema wait %s", dur)
		}

		var step *Step
		var ok bool
		select {
		case step, ok = <-b.plan.q:
			if !ok {
				clog.Infof(ctx, "q is closed")
				done(nil)
				break loop
			}
		case err := <-errch:
			done(err)
			var shouldFail bool
			if err != nil {
				clog.Infof(ctx, "err from errch: %v", err)
				shouldFail = b.failures.shouldFail(err)
			}
			numServs := b.stepSema.NumServs()
			hasReady := b.plan.hasReady()
			// no active steps and no ready steps?
			if !stuck {
				stuck = numServs == 0 && !hasReady
			}
			if log.V(1) {
				clog.Infof(ctx, "errs=%d numServs=%d hasReady=%t stuck=%t", b.failures.n, numServs, hasReady, stuck)
			}
			if shouldFail || stuck {
				clog.Infof(ctx, "unable to proceed nerrs=%d numServs=%d hasReady=%t stuck=%t", b.failures.n, numServs, hasReady, stuck)
				cancel()
				break loop
			}
			continue
		case <-ctx.Done():
			clog.Infof(ctx, "context done")
			done(context.Cause(ctx))
			cancel()
			b.plan.dump(ctx, b.graph)
			return context.Cause(ctx)
		}
		b.plan.pushReady()
		wg.Add(1)
		go func(step *Step) {
			defer wg.Done()
			var err error
			defer func() {
				// send errch after done is called. b/297301112
				select {
				case <-ctx.Done():
				case errch <- err:
				}
			}()
			defer func() {
				if r := recover(); r != nil {
					const size = 64 << 10
					buf := make([]byte, size)
					buf = buf[:runtime.Stack(buf, false)]
					var out string
					if outs := step.def.Outputs(ctx); len(outs) > 0 {
						out = outs[0]
					} else {
						out = fmt.Sprintf("%p", step)
					}
					loc := panicLocation(buf)
					clog.Errorf(ctx, "runStep panic: %v\nstep: %s\n%s", r, out, loc)
					clog.Warningf(ctx, "%s", buf)
					err = fmt.Errorf("panic: %v: %s", r, loc)
				}
			}()
			defer done(nil)

			err = b.runStep(ctx, step)
			select {
			case <-ctx.Done():
				clog.Infof(ctx, "context done")
				return
			default:
			}
		}(step)
	}
	clog.Infof(ctx, "all pendings becomes ready")
	errdone := make(chan error)
	go func() {
		var canceled bool
		for e := range errch {
			b.failures.shouldFail(e)
			if errors.Is(e, context.Canceled) {
				canceled = true
			}
		}
		// step error is already reported in run_step.
		// report errStuck if it coulddn't progress.
		// otherwise, report just number of errors.
		if b.failures.n == 0 {
			if canceled {
				errdone <- context.Cause(ctx)
				return
			}
			errdone <- nil
			return
		}
		if stuck {
			errdone <- fmt.Errorf("cannot make progress due to previous %d errors: %w", b.failures.n, b.failures.firstErr)
			return
		}
		errdone <- fmt.Errorf("%d steps failed: %w", b.failures.n, b.failures.firstErr)
	}()
	wg.Wait()
	close(errch)
	err = <-errdone
	if !b.verbose {
		// The tick draws a 7 row frame every 100ms (summary + 5 step
		// rows + trailing blank). The final message below is 1 line,
		// and PrintLines only clears as many rows as it writes, so
		// printing "build finished" through it would overwrite only
		// the summary row and leave the rest of the frame on screen:
		//
		//   build finished                  (overwritten)
		//     2.1s [exec] clang++ foo.cc    (stale)
		//     1.4s [exec] clang++ bar.cc    (stale)
		//     0.8s [fetch] libc.a           (stale)
		//     0.2s [exec] clang++ baz.cc    (stale)
		//                                   (stale pad row)
		//                                   (stale trailing blank)
		//
		// Stop the tick first so it cannot overdraw us, then
		// clearFrame wipes all 7 rows before we print.
		b.progress.stop()
		b.progress.clearFrame()
		if err == nil {
			ui.Default.PrintLines(fmt.Sprintf("%s finished", name))
		} else {
			ui.Default.PrintLines(fmt.Sprintf("%s failed", name))
		}
	}
	// metrics for full build session, without step_id etc.
	var metrics StepMetric
	metrics.BuildID = b.id
	metrics.Duration = IntervalMetric(time.Since(b.start))
	metrics.Err = err != nil
	b.recordMetrics(ctx, metrics)
	clog.Infof(ctx, "%s finished: %v", name, err)
	if b.rebuildManifest == "" && !b.fastExit && b.failureSummaryWriter != nil {
		// fastExit should not trigger this check at the end of build.
		// also check it uses failureSummaryWriter (which is mainly
		// used on builder only), as we want to check this builder only.
		// developer would kill/interrupt if it won't finish.
		finished := time.Now()
		go func() {
			time.Sleep(10 * time.Minute)
			// expect siso process finishes before it runs
			ui.Default.Errorf("\nBUG: http://b/360961799 - siso didn't finish in %s after build finished \ndump all goroutines:\n", time.Since(finished))
			err := pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
			if err != nil {
				ui.Default.Errorf("failed to WriteTo: %v\n", err)
			}
			ui.Default.Warningf("\nwait more 10 minutes.\n")
			time.Sleep(10 * time.Minute)
			ui.Default.Errorf("siso still didn't finish in %s after build finished \ndump all goroutines:\n", time.Since(finished))
			err = pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
			if err != nil {
				ui.Default.Errorf("failed to WriteTo: %v\n", err)
			}
			os.Exit(1)
		}()
	}
	return err
}

func (b *Builder) uploadBuildNinja(ctx context.Context) {
	started := time.Now()
	inputs := b.graph.Filenames()
	inputs = append(inputs, "args.gn")
	ents, err := b.hashFS.Entries(ctx, b.path.AbsBase(), inputs)
	if err != nil {
		clog.Warningf(ctx, "failed to get build files entries: %v", err)
		return
	}
	ds := digest.NewStore()
	tree := merkletree.NewPooled(ds)
	defer tree.Release()
	for _, ent := range ents {
		err := tree.Set(ent)
		if err != nil {
			clog.Warningf(ctx, "failed to set %s: %v", ent.Name, err)
		}
	}
	d, err := tree.Build(ctx)
	if err != nil {
		clog.Warningf(ctx, "failed to calculate tree: %v", err)
		return
	}
	_, err = b.reapiclient.UploadAll(ctx, ds)
	if err != nil {
		clog.Warningf(ctx, "failed to upload build files tree %s: %v", d, err)
		return
	}
	if b.resultstoreUploader != nil {
		err := b.resultstoreUploader.SetFile(ctx, "build.ninja.dir", d)
		if err != nil {
			clog.Warningf(ctx, "failed to set build files tree %s: %v", d, err)
		}
	}
	clog.Infof(ctx, "uploaded build files tree %s (%d entries) in %s", d, len(ents), time.Since(started))
}

func (b *Builder) recordMetrics(ctx context.Context, m StepMetric) {
	mb, err := json.Marshal(m)
	if err != nil {
		clog.Warningf(ctx, "metrics marshal err: %v", err)
		return
	}
	fmt.Fprintf(b.metricsJSONWriter, "%s\n", mb)
}

func (b *Builder) recordCloudMonitoringActionMetrics(ctx context.Context, step *Step, actionErr error) {
	ar, cached := step.cmd.ActionResult()
	var remoteAr *rpb.ActionResult
	var remoteErr error
	if step.metrics.IsRemote {
		remoteAr = ar
		remoteErr = actionErr
	} else if step.metrics.Fallback {
		remoteAr, remoteErr = step.cmd.RemoteFallbackResult()
	}
	monitoring.ExportActionMetrics(
		ctx, time.Duration(step.metrics.Duration), ar, remoteAr, actionErr, remoteErr, cached, step.metrics.Fallback)
}

func (b *Builder) recordNinjaLogs(ctx context.Context, s *Step) {
	// TODO: b/298594790 - Use the same mtime with hashFS.
	start := time.Duration(s.metrics.ActionStartTime).Milliseconds()
	end := time.Duration(s.metrics.ActionEndTime).Milliseconds()
	if start == end {
		// Artificially forward the end time to give a minimal duration for visibility
		// in trace viewers that do not properly handle 0 duration steps.
		end++
	}

	// Remove prefixed working directory path from Outputs.
	outputs := make([]string, 0, len(s.cmd.Outputs))
	outDir := s.cmd.WorkDir + "/"
	for _, output := range s.cmd.Outputs {
		outputs = append(outputs, strings.TrimPrefix(output, outDir))
	}
	ninjautil.WriteNinjaLogEntries(ctx, b.ninjaLogWriter, start, end, s.endTime, outputs, s.cmd.Args)
}

// stepLogEntry logs step in parent access log of the step.
func stepLogEntry(ctx context.Context, logger *clog.Logger, step *Step, duration time.Duration, err error) {
	httpStatus := http.StatusOK
	logEntry := logger.Entry(logging.Info, fmt.Sprintf("%s -> %v", stepDescription(step.def), err))
	if isCanceled(ctx, err) {
		logEntry.Severity = logging.Warning
		// https://cloud.google.com/apis/design/errors#handling_errors
		httpStatus = 499 // Client closed request
	} else if err != nil {
		logEntry.Severity = logging.Warning
		httpStatus = http.StatusBadRequest
	}
	logEntry.HTTPRequest = &logging.HTTPRequest{
		Request: &http.Request{
			Method: http.MethodPost,
			URL: &url.URL{
				Path: path.Join("/step", step.def.ActionName(), filepath.ToSlash(step.def.Outputs(ctx)[0])),
			},
		},
		Status: httpStatus,
		// RequestSize
		// ResponseSize
		Latency: duration,
		// CacheHit
	}
	logEntry.Labels = map[string]string{
		"id":        step.def.String(),
		"siso_rule": step.metrics.Rule,
		"action":    step.metrics.Action,
		"output":    step.metrics.Output(),
		"gn_target": step.metrics.GNTarget,
		"cmdhash":   step.metrics.CmdHash,
		"prev":      step.metrics.PrevStepID,
		"digest":    step.metrics.Digest,
	}
	if step.metrics.WorkerPool != "" {
		logEntry.Labels["worker_pool"] = step.metrics.WorkerPool
	}
	if step.metrics.RunTime > 0 {
		logEntry.Labels["run_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.RunTime).Seconds())
	}
	if step.metrics.ExecTime > 0 {
		logEntry.Labels["exec_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.ExecTime).Seconds())
	}
	if step.metrics.WorkerTime > 0 {
		logEntry.Labels["worker_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.WorkerTime).Seconds())
	}
	if step.metrics.QueueTime > 0 {
		logEntry.Labels["queue_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.QueueTime).Seconds())
	}
	if step.metrics.DepsScanTime > 0 {
		logEntry.Labels["depsscan_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.DepsScanTime).Seconds())
	}
	if step.metrics.NoExec {
		logEntry.Labels["no_exec"] = "true"
	}
	if step.metrics.IsRemote {
		logEntry.Labels["is_remote"] = "true"
	}
	if step.metrics.IsLocal {
		logEntry.Labels["is_local"] = "true"
	}
	if step.metrics.FastLocal {
		logEntry.Labels["fast_local"] = "true"
	}
	if step.metrics.StartLocal {
		logEntry.Labels["start_local"] = "true"
	}
	if step.metrics.Cached {
		logEntry.Labels["cached"] = "true"
	}
	if step.metrics.Fallback {
		logEntry.Labels["fallback"] = "true"
	}
	if step.metrics.MaxRSS > 0 {
		logEntry.Labels["max_rss"] = strconv.FormatInt(step.metrics.MaxRSS, 10)
	}
	if step.metrics.InputFetchTime > 0 {
		logEntry.Labels["input_fetch_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.InputFetchTime).Seconds())
	}
	if step.metrics.OutputUploadTime > 0 {
		logEntry.Labels["output_upload_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.OutputUploadTime).Seconds())
	}
	// TODO: record more useful metrics
	logger.Log(logEntry)
}

func isCanceled(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	st, ok := status.FromError(err)
	if ok {
		if st.Code() == codes.Canceled {
			return true
		}
	}
	select {
	case <-ctx.Done():
		return true
	default:
	}
	return false
}

// dedupInputs deduplicates inputs.
// For windows worker, which uses case insensitive file system, it also
// deduplicates filenames with different cases, e.g. "Windows.h" vs "windows.h".
// TODO(b/275452106): support Mac worker
func dedupInputs(ctx context.Context, cmd *execute.Cmd) {
	// need to dedup input with different case in intermediate dir on win and mac?
	caseInsensitive := cmd.Platform["OSFamily"] == "Windows"
	m := make(map[string]string, len(cmd.Inputs))
	lenBefore := len(cmd.Inputs)
	inputs := cmd.Inputs[:0]
	for _, input := range cmd.Inputs {
		key := input
		if caseInsensitive {
			key = strings.ToLower(input)
		}
		if s, found := m[key]; found {
			if log.V(1) {
				clog.Infof(ctx, "dedup input %s (%s)", input, s)
			}
			continue
		}
		m[key] = input
		inputs = append(inputs, input)
	}
	lenAfter := len(inputs)
	if lenBefore != lenAfter {
		cmd.Inputs = slices.Clone(inputs)
	} else {
		cmd.Inputs = inputs
	}
}

// outputs processes step's outputs.
// it will flush outputs to local disk if
// - it is specified in local outputs of StepDef.
// - it has an extension that requires scan deps of future steps.
// - it is specified by OutptutLocalFunc.
func (b *Builder) outputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "outputs")
	defer span.Close(nil)

	outputs := step.cmd.Outputs
	if step.def.Binding("phony_output") != "" {
		clog.Infof(ctx, "phony_output. no check output files %q", outputs)
		return nil
	}

	span.SetAttr("outputs", len(outputs))
	if step.cmd.Depfile != "" {
		switch step.cmd.Deps {
		case "gcc", "msvc":
			// for deps=gcc,msvc, ninja will record it in
			// deps log and remove depfile.
		default:
			outputs = append(outputs, step.cmd.Depfile)
		}
	}

	localOutputs := step.def.LocalOutputs(ctx)
	span.SetAttr("outputs-local", len(localOutputs))
	seen := make(map[string]bool)
	for _, o := range localOutputs {
		if seen[o] {
			continue
		}
		seen[o] = true
	}

	clog.Infof(ctx, "outputs %d->%d", len(outputs), len(localOutputs))
	defOutputs := step.def.Outputs(ctx)
	// need to check against step.cmd.Outputs, not step.def.Outputs, since
	// handler may add to step.cmd.Outputs.
	for _, out := range outputs {
		if seen[out] {
			continue
		}
		seen[out] = true
		var local bool
		fullOut := out
		if !filepath.IsAbs(fullOut) {
			fullOut = filepath.Join(step.cmd.WorkspaceRoot, out)
		}
		if b.outputLocal != nil && b.outputLocal(ctx, out) {
			localOutputs = append(localOutputs, out)
			local = true
		} else {
			// check if it already exists on local.
			// if so, better to flush to the disk
			// as other future step would access it locally.
			_, err := b.hashFS.OS.Lstat(ctx, fullOut)
			if err == nil {
				clog.Infof(ctx, "output_local=false but local exists: %q", out)
				localOutputs = append(localOutputs, out)
				local = true
			}
		}
		fi, err := b.hashFS.Stat(ctx, step.cmd.WorkspaceRoot, out)
		if err != nil {
			b.targets.Store(out, targetState{
				dirtyErr: err,
			})
			reqOut := slices.Contains(defOutputs, out)
			if reqOut {
				if experiments.Enabled("ignore-missing-outputs", "") {
					b.hashFS.AddMissingOutput(ctx, step.cmd.WorkspaceRoot, out)
				} else {
					return fmt.Errorf("missing outputs %s: %w", out, err)
				}
			}
			clog.Warningf(ctx, "missing outputs %s: %v", out, err)
			if !local {
				// need to make sure it doesn't exist on disk too
				// for local=true, Flush will remove.
				err = b.hashFS.OS.Remove(ctx, fullOut)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					clog.Warningf(ctx, "remove missing outputs %q: %v", out, err)
				}
			}
			continue
		}
		b.targets.Store(out, targetState{
			mtime:   fi.ModTime(),
			changed: fi.IsChanged(),
		})
	}
	if len(localOutputs) > 0 {
		err := b.hashFS.Flush(ctx, step.cmd.WorkspaceRoot, localOutputs)
		if err != nil {
			return fmt.Errorf("%w: %w", errFlushOutput, err)
		}
	}
	return nil
}

// progressStepCacheHit shows progress of the cache hit step.
func (b *Builder) progressStepCacheHit(step *Step) {
	b.progress.step(b, step, progressPrefixCacheHit+step.cmd.Desc)
}

// progressStepStarted shows progress of the started step.
func (b *Builder) progressStepStarted(step *Step) {
	step.setPhase(stepStart)
	b.progress.step(b, step, progressPrefixStart+step.cmd.Desc)
}

// progressStepFinished shows progress of the finished step.
func (b *Builder) progressStepFinished(step *Step) {
	step.setPhase(stepDone)
	b.progress.step(b, step, progressPrefixFinish+step.cmd.Desc)
}

// progressStepError shows progress of the error step.
func (b *Builder) progressStepError(step *Step) {
	step.setPhase(stepDone)
	b.progress.step(b, step, progressPrefixError+step.cmd.Desc)
}

// progressStepCanceled shows progress of the canceled step.
func (b *Builder) progressStepCanceled(step *Step) {
	step.setPhase(stepDone)
	b.progress.step(b, step, progressPrefixCanceled+step.cmd.Desc)
}

// progressStepRetry shows progress of the retried step.
func (b *Builder) progressStepRetry(step *Step) {
	b.progress.step(b, step, progressPrefixRetry+step.cmd.Desc)
}

// progressStepFallback shows progress of the fallback step.
func (b *Builder) progressStepFallback(step *Step) {
	b.progress.step(b, step, progressPrefixFallback+step.cmd.Desc)
}

// progressStepCacheWrite shows progress of the cache-write step.
func (b *Builder) progressStepCacheWrite(step *Step) {
	b.progress.step(b, step, progressPrefixCacheWrite+step.cmd.Desc)
}

var errNotRelocatable = errors.New("request is not relocatable")
var errNotInsideWorkspace = errors.New("inputs are not inside workspace")
var errFlushOutput = errors.New("failed to flush outputs to local")

func (b *Builder) updateDeps(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "update-deps")
	defer span.Close(nil)
	if len(step.cmd.Outputs) == 0 {
		clog.Warningf(ctx, "update deps: no outputs")
		return nil
	}
	output, err := filepath.Rel(step.cmd.WorkDir, step.cmd.Outputs[0])
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to get rel %s,%s: %v", step.cmd.WorkDir, step.cmd.Outputs[0], err)
		return nil
	}
	fi, err := b.hashFS.Stat(ctx, step.cmd.WorkspaceRoot, step.cmd.Outputs[0])
	if err != nil {
		clog.Warningf(ctx, "update deps: missing outputs %s: %v", step.cmd.Outputs[0], err)
		return nil
	}
	ents, err := b.hashFS.Entries(ctx, step.cmd.WorkspaceRoot, []string{step.cmd.Outputs[0]})
	if err != nil || len(ents) == 0 {
		clog.Warningf(ctx, "update deps: failed to get output entry %q %d: %v", step.cmd.Outputs[0], len(ents), err)
		return nil
	}
	deps, err := depsAfterRun(ctx, b, step)
	if err != nil {
		return err
	}
	updated, err := step.def.RecordDeps(ctx, output, fi.ModTime(), ents[0].Data.Digest(), deps)
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to record deps %s, %s, %s, %s, %s: %v", output, base64.StdEncoding.EncodeToString(step.cmd.CmdHash), fi.ModTime(), ents[0].Data.Digest(), deps, err)
	}
	clog.Infof(ctx, "update deps=%s: %s %s %d updated:%t pure:%t/%t->true", step.cmd.Deps, output, base64.StdEncoding.EncodeToString(step.cmd.CmdHash), len(deps), updated, step.cmd.Pure, step.cmd.Pure)
	span.SetAttr("deps", len(deps))
	span.SetAttr("updated", updated)
	canonicalizedDeps := make([]string, 0, len(deps))
	for _, dep := range deps {
		canonicalizedDeps = append(canonicalizedDeps, b.path.MaybeFromRelative(ctx, dep))
	}
	depsFixCmd(ctx, b, step, canonicalizedDeps)
	return nil
}

// TraceEnabled reports whether tracing is enabled.
func (b *Builder) TraceEnabled() bool {
	if b == nil {
		return false
	}
	return b.tracer.Enabled() || b.traceExporter != nil || b.tracePprof.Enabled()
}

func (b *Builder) finalizeTrace(ctx context.Context, tc *trace.Context) {
	if tc == nil {
		return
	}
	b.tracer.Record(b.traceEvents(ctx, tc))
	b.traceStats.update(tc)
	b.traceExporter.Export(ctx, tc)
	b.tracePprof.Add(ctx, tc)
}

func (b *Builder) ActiveSteps() []ActiveStepInfo {
	return b.progress.ActiveSteps()
}

func (b *Builder) localFallbackEnabled() bool {
	return !b.strictRemote && b.maxFallbackAllowed > 0 && !b.hashFS.OnCog()
}
