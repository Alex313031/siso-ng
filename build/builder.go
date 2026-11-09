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

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/build/metadata"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/localexec"
	"go.chromium.org/build/siso/execute/remoteexec"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/hashfs/osfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/o11y/monitoring"
	sisopprof "go.chromium.org/build/siso/o11y/pprof"
	"go.chromium.org/build/siso/o11y/trace"
	sisopath "go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi"
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

// A ResultSink records results of the invocation.
// It implements io.Writer for writing a transcript of the invocation.
type ResultSink interface {
	io.Writer
	// UploadBuildNinja uploads Build Ninja files to this result sink.
	UploadBuildNinja(ctx context.Context, d digest.Digest) error
	// UploadBuildConfig records and uploads config files to this result sink.
	// Can be called again when manifest reloaded, which regenerates the configuration.
	UploadBuildConfig(ctx context.Context, properties map[string]string, ents []merkletree.Entry) error
}

// chromium recipe module expects this string.
const ninjaNoWorkToDo = "ninja: no work to do.\n"

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
	ActionSalt         []byte
	LocalActionSalt    []byte

	LocallyNeeded        *LocallyNeededSet
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
	PprofUploader        sisopprof.Uploader
	ResultSink           ResultSink

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

	// WeightsFile is an optional path to a JSON(.gz) file specifying positive
	// integer step weights for critical-path step scheduling.
	WeightsFile string

	// BuildLabel to attach to output files
	BuildLabel string

	// MissingDeps specifies how to handle missing dependencies:
	// "ignore" (default), "warn", "error", or "fatal".
	MissingDeps MissingDepsMode
}

// EnvConfig is a config for environment variables.
type EnvConfig struct {
	// Inherits lists environment variable names to inherit from Siso's environment.
	// Mutually exclusive with Excludes.
	Inherits []string `json:"inherits,omitempty"`

	// Excludes lists environment variable names that are not passed to step commands
	// when inheriting Siso's environment. Mutually exclusive with Inherits.
	Excludes []string `json:"excludes,omitempty"`

	// CacheKeyOmits lists environment variable names that are passed to step commands,
	// but not included in cache keys (e.g. two-phase caching).
	CacheKeyOmits []string `json:"cache_key_omits,omitempty"`

	// CacheKeySubstitutions maps strings to their replacements when normalizing
	// environment variable values for cache keys.
	CacheKeySubstitutions map[string]string `json:"cache_key_substitutions,omitempty"`

	// CacheKeyTrimPrefixes lists prefixes to strip from normalized environment
	// variable values when the value does not contain a path list separator.
	CacheKeyTrimPrefixes []string `json:"cache_key_trim_prefixes,omitempty"`
}

// MissingDepsMode specifies how to handle missing dependencies.
type MissingDepsMode string

const (
	MissingDepsIgnore MissingDepsMode = "ignore"
	MissingDepsWarn   MissingDepsMode = "warn"
	MissingDepsError  MissingDepsMode = "error"
	MissingDepsFatal  MissingDepsMode = "fatal"
)

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

	// record target's state, keyed by clean (slash-normalized) target path.
	targets targetMap

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

	actionSalt      []byte
	localActionSalt []byte

	locallyNeeded *LocallyNeededSet

	cacheSema *semaphore.Semaphore
	cache     *Cache

	// two phase caching
	twoPhaseCachingSema *semaphore.Semaphore
	twoPhaseCaching     twoPhaseCaching
	// dirSpecs caches what matchInputRoot derives from a Directory at a
	// path. dirSpecKey -> *dirSpec. See dirspec.go.
	dirSpecs sync.Map

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
	traceDeps            *depRecorder
	tracePprof           *tracePprof
	pprofUploader        sisopprof.Uploader
	resultSink           ResultSink

	tracePidPreproc, tracePidLocal, tracePidRemote, tracePidWorker int64

	// envfiles: filename -> *envfile
	envFiles sync.Map

	// env is the environment variables inherited by local build steps.
	env []string

	// envCacheKeyOmits are environment variable names omitted when recording environment variables in cache keys.
	envCacheKeyOmits map[string]bool

	// envCacheKeySubstitutions maps strings to their replacements when normalizing environment variable values for cache keys.
	envCacheKeySubstitutions map[string]string

	// envCacheKeyTrimPrefixes are prefixes stripped from normalized environment variable values when they do not contain a path list separator.
	envCacheKeyTrimPrefixes []string

	// envHash is a hash of the normalized, non-omitted environment variables in env,
	// used to invalidate local steps on incremental builds when environment variables change.
	envHash []byte

	clobber bool

	fastExit bool

	prepare               bool
	verbose               bool
	verboseFailures       bool
	dryRun                bool
	strictRemote          bool
	UploadBuildNinjaFiles bool

	failures failures

	// hasDeferredMissingDepsErrors tracks whether any step encountered missing
	// dependency errors in "error" mode. Failure is deferred to the end of the build
	// so that all missing dependency errors across the build can be discovered.
	hasDeferredMissingDepsErrors atomic.Bool
	numFallback                  atomic.Int64
	maxFallbackAllowed           int64

	// ninja debug modes
	keepRSP     bool
	keepDepfile bool

	rebuildManifest string

	lastFailureTargets map[string]struct{}
	weightsFile        string

	buildLabel  string
	missingDeps MissingDepsMode
}

// New creates new builder.
func New(ctx context.Context, graph Graph, opts Options) (_ *Builder, err error) {
	logger := clog.FromContext(ctx)
	if logger != nil {
		logger.Formatter = logFormat
	}
	// The hashfs digests become remote-execution cache keys, so the local
	// filesystem and the REAPI backend must hash with the same function.
	if opts.REAPIClient != nil && opts.HashFS.DigestFunction() != opts.REAPIClient.DigestFunction() {
		return nil, fmt.Errorf("content digest function mismatch: hashfs=%s reapi=%s", opts.HashFS.DigestFunction(), opts.REAPIClient.DigestFunction())
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
	if opts.REAPIClient != nil {
		if opts.RECacheEnableWrite && !opts.REAPIClient.UpdateActionResultEnabled() {
			return nil, fmt.Errorf("reapi doesn't support UpdateActionResult, required for --re_cache_enable_write")
		}

		logger.Infof("enable built-in remote exec")
		re = remoteexec.New(ctx, opts.REAPIClient)

	} else {
		logger.Infof("disable built-in remote exec")
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
		actionSalt:         opts.ActionSalt,
		localActionSalt:    opts.LocalActionSalt,
		reapiclient:        opts.REAPIClient,
		reSchedStat:        semaphore.Stat{Name: "re:sched"},
		reWorkerStat:       semaphore.Stat{Name: "re:worker"},

		locallyNeeded: opts.LocallyNeeded,
		cacheSema:     semaphore.New("cache", opts.Limits.Cache),
		cache:         opts.Cache,

		// TODO(b/539291862): revisit limit configuration.
		// This limit is needed to avoid too many concurrent gRPC
		// calls (i.e. protect from gRPC stream saturation).
		twoPhaseCachingSema:   semaphore.New("two-phase-cache", opts.Limits.TwoPhaseCache),
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
		traceDeps:             newDepRecorder(ctx, opts.Tracer),
		tracePprof:            newTracePprof(opts.Pprof),
		pprofUploader:         opts.PprofUploader,
		resultSink:            opts.ResultSink,
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
		weightsFile:           opts.WeightsFile,
		buildLabel:            opts.BuildLabel,
		missingDeps:           opts.MissingDeps,
	}
	if envCfg := graph.EnvConfig(ctx); envCfg != nil {
		if len(envCfg.Inherits) > 0 {
			for _, k := range envCfg.Inherits {
				if v, ok := os.LookupEnv(k); ok {
					b.env = append(b.env, k+"="+v)
				}
			}
		} else {
			excludes := make(map[string]bool, len(envCfg.Excludes))
			for _, k := range envCfg.Excludes {
				excludes[k] = true
			}
			for _, kv := range os.Environ() {
				k, _, _ := strings.Cut(kv, "=")
				if !excludes[k] {
					b.env = append(b.env, kv)
				}
			}
		}
		if len(envCfg.CacheKeyOmits) > 0 {
			b.envCacheKeyOmits = make(map[string]bool, len(envCfg.CacheKeyOmits))
			for _, k := range envCfg.CacheKeyOmits {
				b.envCacheKeyOmits[k] = true
			}
		}
		b.envCacheKeySubstitutions = envCfg.CacheKeySubstitutions
		b.envCacheKeyTrimPrefixes = envCfg.CacheKeyTrimPrefixes
		b.envHash = calculateEnvHash(b.env, b.envCacheKeySubstitutions, b.envCacheKeyTrimPrefixes, b.envCacheKeyOmits)
	}
	for _, t := range opts.LastFailureTargets {
		b.lastFailureTargets[t] = struct{}{}
	}
	if opts.Limits.StartLocal > 0 {
		b.startLocalCounter.Store(int32(opts.Limits.StartLocal))
	}
	if b.reapiclient != nil {
		reapiVer := b.reapiclient.APIVersion()
		clog.Infof(ctx, "reapi version=%v; output_paths=%t action.platform=%t", reapiVer, reapiVer.UseOutputPaths(), reapiVer.UseActionForPlatformProperties())
	}
	if experiments.Enabled("ignore-missing-out-in-depfile", "ignore missing out error in depfile") {
		makeutil.IgnoreMissingOut = true
	}
	if experiments.Enabled("two-phase-caching", "") {
		if b.reapiclient != nil {
			rt := reapiTwoPhaseCaching{
				b:              b,
				actionCacheMap: b.reapiclient.ActionCacheMap(),
			}
			b.twoPhaseCaching = rt
		}
	}
	return b, nil
}

// Close cleans up the builder.
func (b *Builder) Close() error {
	return nil
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
		b.twoPhaseCachingSema.Stat(),
		scandeps.CPPScanSema.Stat(),
		b.fastLocalSema.Stat(),
		b.hashFS.DigestSemaphore().Stat(),
		localexec.ForkSema.Stat(),
		b.hashFS.ActiveFlushGate().Stat(),
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
		b.reapiclient.UploadSemaphore().Stat(),
		b.rewrapSema.Stat(),
		b.remoteSema.Stat(),
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

// ErrMissingDepsViolation is an error to indicate that the build failed due to missing dependency violations.
var ErrMissingDepsViolation = errors.New("missing dependency violations")

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
			_, err = b.hashFS.Stat(ctx, b.path.AbsBase(), sisopath.New(fname))
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
	var sw *stepWeights
	var weightsStart time.Time
	if b.rebuildManifest == "" && b.weightsFile != "" {
		weightsStart = time.Now()
		var werr error
		sw, werr = loadStepWeights(b.weightsFile)
		if werr != nil {
			clog.Warningf(ctx, "failed to load weights file %q: %v", b.weightsFile, werr)
		}
	}
	var failedTargets []Target
	if len(b.lastFailureTargets) > 0 {
		var err error
		failedTargets, err = b.graph.Targets(ctx, slices.Collect(maps.Keys(b.lastFailureTargets))...)
		if err != nil {
			clog.Warningf(ctx, "failed to get last failure targets: %v", err)
		}
	}
	stepWeightFn, matchedWeights := newStepWeightFn(b.graph, sw, failedTargets)
	if sw != nil {
		clog.Infof(ctx, "loaded %d output weights (%d matched graph targets), %d action rules (default=%d) from %q in %s", len(sw.outputs), matchedWeights, len(sw.rules), sw.defaultWeight, b.weightsFile, time.Since(weightsStart))
	}
	schedOpts := schedulerOption{
		NumTargets:    b.graph.NumTargets(),
		Path:          b.path,
		HashFS:        b.hashFS,
		Prepare:       b.prepare,
		StepWeight:    stepWeightFn,
		LocallyNeeded: b.locallyNeeded,
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
		fi, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, sisopath.New(b.path.MaybeFromRelative(ctx, b.rebuildManifest)))
		if err == nil {
			mftime = fi.ModTime()
			clog.Infof(ctx, "manifest %s: %s", b.rebuildManifest, mftime)
		}
	}
	defer func() {
		stat = b.Stats()
		if b.rebuildManifest != "" {
			fi, mferr := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, sisopath.New(b.path.MaybeFromRelative(ctx, b.rebuildManifest)))
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
			fsstat.ROps, fsstat.RErrs, ui.NumBytes(fsstat.RBytes),
			fsstat.WOps, fsstat.WErrs, ui.NumBytes(fsstat.WBytes))
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
				restat.ROps, restat.RErrs, ui.NumBytes(restat.RBytes),
				restat.WOps, restat.WErrs, ui.NumBytes(restat.WBytes))
		}
		msg := fmt.Sprintf("\nnoexec:%d local:%d remote:%d cache:%d skip:%d\n  cache-write:%d(err:%d) fallback:%d retry:%d\n",
			stat.NoExec, stat.Local, stat.Remote, stat.CacheHit, stat.Skipped, stat.CacheWrite, stat.CacheWriteErr, stat.LocalFallback, stat.RemoteRetry) +
			depsStatLine +
			restatLine +
			fsstatLine + "\n"
		ui.Default.PrintLines("\n", msg)
		if b.resultSink != nil {
			fmt.Fprintln(b.resultSink, msg)
		}
	}()
	semas := []trace.Semaphore{
		b.cache.sema,
		b.cacheSema,
		b.twoPhaseCachingSema,
		b.localSema,
		b.remoteSema,
		b.rewrapSema,
		b.stepSema,
		b.hashFS.ActiveFlushGate(),
		hashfs.ForgetMissingsSemaphore,
		osfs.LstatSemaphore,
		reapi.LocalCacheSemaphore,
		reapi.FileSemaphore,
		gccutil.Semaphore,
		msvcutil.Semaphore,
		remoteexec.Semaphore,
	}
	if sema := b.reapiclient.UploadSemaphore(); sema != nil {
		semas = append(semas, sema)
	}
	b.tracer.Start(ctx, semas, []*iometrics.IOMetrics{
		b.hashFS.OS.IOMetrics,
		b.reapiclient.IOMetrics(),
		// TODO: cache iometrics?
	})
	defer func() {
		b.traceDeps.record() // queues events, so before the writer stops
		b.tracer.Stop()
	}()
	b.tracePidPreproc = b.tracer.Process(ctx, "preproc")
	b.tracePidLocal = b.tracer.Process(ctx, "local-exec")
	b.tracePidRemote = b.tracer.Process(ctx, "remote-exec")
	b.tracePidWorker = b.tracer.Process(ctx, "rbe")
	b.tracePprof.SetMetadata(b.metadata)
	if b.pprofUploader != nil {
		b.pprofUploader.SetMetadata(ctx, b.metadata)
	}
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
						out = string(outs[0])
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
			if b.hasDeferredMissingDepsErrors.Load() {
				errdone <- fmt.Errorf("build failed due to %w", ErrMissingDepsViolation)
				return
			}
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
	ents, err := b.hashFS.Entries(ctx, b.path.AbsBase(), sisopath.Paths(inputs))
	if err != nil {
		clog.Warningf(ctx, "failed to get build files entries: %v", err)
		return
	}
	ds := blob.NewStore()
	tree := merkletree.NewPooled(b.hashFS.DigestFunction(), ds)
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
	if b.resultSink != nil {
		err := b.resultSink.UploadBuildNinja(ctx, d)
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
	outputs := make([]string, 0, len(s.cmd.Outputs)+len(s.cmd.OutputDirs))
	outDir := string(s.cmd.WorkDir) + "/"
	for _, output := range s.cmd.Outputs {
		outputs = append(outputs, strings.TrimPrefix(string(output), outDir))
	}
	for _, output := range s.cmd.OutputDirs {
		// Re-append trailing "/" to match ninja's target name convention.
		outputs = append(outputs, strings.TrimPrefix(string(output), outDir)+"/")
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
				Path: path.Join("/step", step.def.ActionName(), string(step.def.Outputs(ctx)[0])),
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
	if step.metrics.ScandepsTime > 0 {
		logEntry.Labels["scandeps_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.ScandepsTime).Seconds())
	}
	if step.metrics.CacheTime > 0 {
		logEntry.Labels["cache_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.CacheTime).Seconds())
	}
	if step.metrics.MaterializeInputsTime > 0 {
		logEntry.Labels["materialize_inputs_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.MaterializeInputsTime).Seconds())
	}
	if step.metrics.MaterializeOutputsTime > 0 {
		logEntry.Labels["materialize_outputs_secs"] = fmt.Sprintf("%.02f", time.Duration(step.metrics.MaterializeOutputsTime).Seconds())
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

// caseInsensitiveDedupMapPool pools dedupInputs' collapse map.
var caseInsensitiveDedupMapPool = sync.Pool{
	New: func() any { return make(map[sisopath.Path]sisopath.Path, 1024) },
}

// dedupInputs collapses case-insensitive duplicates in cmd.Inputs for
// Windows remote workers ("Windows.h" vs "windows.h" on NTFS); inputs
// for other workers are already unique case-sensitively.
// TODO(b/275452106): support Mac worker.
func dedupInputs(ctx context.Context, cmd *execute.Cmd) {
	if cmd.Platform["OSFamily"] != "Windows" {
		return
	}
	m := caseInsensitiveDedupMapPool.Get().(map[sisopath.Path]sisopath.Path)
	defer func() {
		clear(m)
		caseInsensitiveDedupMapPool.Put(m)
	}()
	lenBefore := len(cmd.Inputs)
	inputs := cmd.Inputs[:0]
	for _, input := range cmd.Inputs {
		key := sisopath.Path(strings.ToLower(string(input)))
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
			// Clone first: outputs may share cmd.Outputs' backing array
			// (spare capacity), so a bare append could corrupt it.
			outputs = append(slices.Clone(outputs), step.cmd.Depfile)
		}
	}

	defLocalOutputs := step.def.LocalOutputs(ctx)
	span.SetAttr("outputs-local", len(defLocalOutputs))
	seen := make(map[sisopath.Path]bool)
	var localOutputs []sisopath.Path
	for _, o := range defLocalOutputs {
		if seen[o] {
			continue
		}
		seen[o] = true
		localOutputs = append(localOutputs, o)
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
		fullOut := string(out)
		if !filepath.IsAbs(fullOut) {
			fullOut = filepath.Join(step.cmd.WorkspaceRoot, fullOut)
		}
		if b.hashFS.NeedFlush(ctx, step.cmd.WorkspaceRoot, out) {
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
			b.targets.Store(string(out), targetState{
				dirtyErr: err,
			})
			reqOut := isRequiredOutput(out, defOutputs)
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
		b.targets.Store(string(out), targetState{
			mtime:   fi.ModTime(),
			changed: fi.IsChanged(),
		})
	}
	// Process directory outputs. A missing dir output is treated like a
	// missing file output: hard-fail when declared (required), warn otherwise.
	dirFsys := b.hashFS.FileSystem(ctx, step.cmd.WorkspaceRoot)
	for _, dir := range step.cmd.OutputDirs {
		targetKey := string(dir)
		// A flush only adds files, so prune members the new tree dropped (the
		// local exec path instead wipes pre-run via ResetDirOutput). Prune
		// before anything walks the tree: a walk picks up still-on-disk files
		// into hashfs, so a walk before the prune would count the stale
		// members as part of the directory output. Not gated on NeedFlush: a
		// tree materialized by an earlier build must be pruned even when this
		// build leaves materialization to a consumer, because the on-demand
		// flush walks the tree (expandFlushDirs) and nothing prunes there.
		if err := b.hashFS.RemoveStaleDirMembers(ctx, step.cmd.WorkspaceRoot, string(dir)); err != nil {
			return fmt.Errorf("prune stale dir output %s: %w", dir, err)
		}
		// Flush when output_local requests it; otherwise consumers materialize
		// the current tree on demand. Like a file output, a tree that already
		// exists on disk (an earlier build or a local consumer materialized
		// it) is flushed too: the prune above only removes dropped members,
		// so without a flush members whose contents changed would keep their
		// old contents on disk.
		local := b.hashFS.NeedFlush(ctx, step.cmd.WorkspaceRoot, dir)
		if !local {
			fullDir := sisopath.JoinRoot(step.cmd.WorkspaceRoot, dir)
			if _, err := b.hashFS.OS.Lstat(ctx, string(fullDir)); err == nil {
				clog.Infof(ctx, "output_local=false but local dir exists: %q", dir)
				local = true
			}
		}
		fi, err := b.hashFS.Stat(ctx, step.cmd.WorkspaceRoot, dir)
		if err != nil {
			b.targets.Store(targetKey, targetState{
				dirtyErr: err,
			})
			if isRequiredOutput(dir, defOutputs) {
				if experiments.Enabled("ignore-missing-outputs", "") {
					b.hashFS.AddMissingOutput(ctx, step.cmd.WorkspaceRoot, dir)
				} else {
					return fmt.Errorf("missing dir output %s: %w", dir, err)
				}
			}
			clog.Warningf(ctx, "missing dir output %s: %v", dir, err)
			continue
		}
		mtime, walkErr := effectiveMtime(dirFsys, string(dir), fi.ModTime(), true)
		if walkErr != nil {
			b.targets.Store(targetKey, targetState{
				dirtyErr: walkErr,
			})
			// Can't fully read the output: hard-fail if required, else warn.
			if isRequiredOutput(dir, defOutputs) && !experiments.Enabled("ignore-missing-outputs", "") {
				return fmt.Errorf("walk dir output %s: %w", dir, walkErr)
			}
			clog.Warningf(ctx, "walk dir output %s: %v", dir, walkErr)
		} else {
			b.targets.Store(targetKey, targetState{
				mtime:   mtime,
				changed: fi.IsChanged(),
			})
		}
		if local {
			// Trailing slash marks a declared directory artifact so Flush
			// materializes its whole tree, not just the directory node.
			localOutputs = append(localOutputs, dir+"/")
		}
	}
	if len(localOutputs) > 0 {
		_, mspan := trace.NewSpan(ctx, spanMaterializeOutputs)
		err := b.hashFS.Flush(ctx, step.cmd.WorkspaceRoot, localOutputs)
		mspan.Close(nil)
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
	if len(step.cmd.Outputs) == 0 || len(step.outputPaths) == 0 {
		// A directory-only edge cannot carry a depfile or deps.
		if step.cmd.Deps != "" || step.cmd.Depfile != "" {
			return fmt.Errorf("update deps: %s declares deps=%q depfile=%q but has only directory outputs %v; deps require a file output to key on", step, step.cmd.Deps, step.cmd.Depfile, step.cmd.OutputDirs)
		}
		clog.Warningf(ctx, "update deps: no outputs")
		return nil
	}
	outputInNinja := step.outputPaths[0]
	outputPath := sisopath.Path(b.path.MaybeFromRelative(ctx, outputInNinja))
	fi, err := b.hashFS.Stat(ctx, step.cmd.WorkspaceRoot, outputPath)
	if err != nil {
		clog.Warningf(ctx, "update deps: missing outputs %s: %v", outputPath, err)
		return nil
	}
	ents, err := b.hashFS.Entries(ctx, step.cmd.WorkspaceRoot, []sisopath.Path{outputPath})
	if err != nil || len(ents) == 0 {
		clog.Warningf(ctx, "update deps: failed to get output entry %q %d: %v", outputPath, len(ents), err)
		return nil
	}
	deps, err := depsAfterRun(ctx, b, step)
	if err != nil {
		return err
	}
	updated, err := step.def.RecordDeps(ctx, outputInNinja, fi.ModTime(), ents[0].Data.Digest(), deps)
	if err != nil {
		clog.Warningf(ctx, "update deps: failed to record deps %s, %s, %s, %s, %s: %v", outputInNinja, base64.StdEncoding.EncodeToString(step.cmd.CmdHash), fi.ModTime(), ents[0].Data.Digest(), deps, err)
	}
	clog.Infof(ctx, "update deps=%s: %s %s %d updated:%t pure:%t/%t->true", step.cmd.Deps, outputInNinja, base64.StdEncoding.EncodeToString(step.cmd.CmdHash), len(deps), updated, step.cmd.Pure, step.cmd.Pure)
	span.SetAttr("deps", len(deps))
	span.SetAttr("updated", updated)
	canonicalizedDeps := make([]sisopath.Path, 0, len(deps))
	for _, dep := range deps {
		canonicalizedDeps = append(canonicalizedDeps, b.path.FromRelativePath(ctx, dep))
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

func (b *Builder) finalizeTrace(ctx context.Context, tc *trace.Context, step *Step) {
	if tc == nil {
		return
	}
	b.tracer.Record(b.traceEvents(ctx, tc, step))
	b.traceStats.update(tc)
	b.traceExporter.Export(ctx, tc)
	b.tracePprof.Add(ctx, tc)
}

func (b *Builder) ActiveSteps() []ActiveStepInfo {
	return b.progress.ActiveSteps()
}

func (b *Builder) ProgressInfo() ProgressInfo {
	stat := b.stats.stats()
	return ProgressInfo{
		Done:        stat.Done - stat.Skipped,
		Total:       stat.Total - stat.Skipped,
		Skipped:     stat.Skipped,
		ActiveSteps: b.ActiveSteps(),
	}
}

func (b *Builder) localFallbackEnabled(step *Step) bool {
	if step != nil && step.def.Binding("strict_remote") == "true" {
		return false
	}
	return !b.strictRemote && b.maxFallbackAllowed > 0 && !b.hashFS.OnCog()
}

func (b *Builder) missingDepsCheckEnabled() bool {
	return b.missingDepsMode() != MissingDepsIgnore
}

func (b *Builder) missingDepsMode() MissingDepsMode {
	switch b.missingDeps {
	case MissingDepsIgnore, MissingDepsWarn, MissingDepsError, MissingDepsFatal:
		return b.missingDeps
	}
	return MissingDepsIgnore
}
