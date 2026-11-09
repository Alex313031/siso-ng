// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/anypb"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/execute/localexec"
	epb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// mockTapHelper wraps localexec.Spawner and attaches a TapResult into
// ActionResult.ExecutionMetadata.AuxiliaryMetadata.
type mockTapHelper struct {
	localexec.Spawner
}

func (h mockTapHelper) Run(ctx context.Context, req *epb.SpawnRequest) (*epb.SpawnResult, error) {
	res, err := h.Spawn(ctx, req)
	if err != nil {
		return nil, err
	}
	ar := &rpb.ActionResult{}
	if err := res.ActionResult.UnmarshalTo(ar); err != nil {
		return nil, err
	}
	if ar.ExitCode != 0 {
		return res, nil
	}
	cwd := filepath.Join(req.GetWorkspaceRoot(), req.GetWorkDir())
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Clean(filepath.Join(cwd, p))
	}
	var reads, writes, deletes []string
	for _, arg := range req.GetArgs()[1:] {
		if v, ok := strings.CutPrefix(arg, "--output="); ok {
			writes = append(writes, abs(v))
		} else if v, ok := strings.CutPrefix(arg, "--unlink-output="); ok {
			writes = append(writes, abs(v))
			deletes = append(deletes, abs(v))
		} else if v, ok := strings.CutPrefix(arg, "--temp-file="); ok {
			writes = append(writes, abs(v))
			deletes = append(deletes, abs(v))
		} else if v, ok := strings.CutPrefix(arg, "--mkdir="); ok {
			writes = append(writes, abs(v))
		} else if v, ok := strings.CutPrefix(arg, "--check-dir="); ok {
			reads = append(reads, abs(v))
		} else if v, ok := strings.CutPrefix(arg, "--check-file="); ok {
			reads = append(reads, abs(v))
		} else if !strings.HasPrefix(arg, "-") {
			reads = append(reads, abs(arg))
		}
	}
	tapData := &epb.TapResult{
		Reads:   reads,
		Writes:  writes,
		Deletes: deletes,
	}
	anyTap, err := anypb.New(tapData)
	if err != nil {
		return nil, err
	}
	if ar.ExecutionMetadata == nil {
		ar.ExecutionMetadata = &rpb.ExecutedActionMetadata{}
	}
	ar.ExecutionMetadata.AuxiliaryMetadata = append(ar.ExecutionMetadata.AuxiliaryMetadata, anyTap)
	anyRes, err := anypb.New(ar)
	if err != nil {
		return nil, err
	}
	return &epb.SpawnResult{ActionResult: anyRes}, nil
}

func TestBuild_TwoPhaseCaching_RestatContent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching is available on linux only")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	t.Setenv("TMPDIR", tempDir(t))
	t.Setenv("TMP", tempDir(t))

	build.SetExperimentForTest("two-phase-caching")

	exists := func(fname string) error {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		return err
	}

	content := []byte("output of foo.o\n")
	depContent := []byte("foo.o: ../../base/foo.cc ../../base/foo.h\n")
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			dg, err := fakere.Put(ctx, content)
			if err != nil {
				return nil, err
			}
			depDg, err := fakere.Put(ctx, depContent)
			if err != nil {
				return nil, err
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "foo.o",
						Digest: dg,
					},
					{
						Path:   "foo.o.d",
						Digest: depDg,
					},
				},
			}, nil
		},
	}
	// keep global datasource to keep mock REAPI server.
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	localCache, err := reapi.NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ds.Client = reapitest.NewWithOption(ctx, t, fakere, reapi.Option{
		LocalCache:                    localCache,
		DisableTwoPhaseCachingMethods: true,
	})
	err = ds.Client.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds.Cache = ds.Client.CacheStore()

	runNinja := func(ctx context.Context, t *testing.T, metricsBuffer *syncBuffer) (build.Stats, error) {
		t.Helper()
		hashfsOpts := hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			DataSource:  ds,
		}
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.Cache,
			EnableRead: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.RECacheEnableWrite = true
		opt.REAPIClient = ds.Client
		opt.REExecEnable = true
		if metricsBuffer != nil {
			opt.MetricsJSONWriter = metricsBuffer
		}
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	func() {
		t.Logf("--- first build")
		_, err := runNinja(ctx, t, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := exists("foo.o"); err != nil {
			t.Errorf("foo.o doesn't exist: %v", err)
		}
		if err := exists("bar.out"); err != nil {
			t.Errorf("bar.out doesn't exist: %v", err)
		}
	}()

	func() {
		t.Logf("--- second build: touch base/foo.h, expect two-phase cache hit for foo.o and bar.out skipped")
		touchFile(t, dir, "base/foo.h")
		var metricsBuffer syncBuffer
		stat, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 2 { // all(phony) and bar.out
			t.Errorf("Skipped=%d; want 2", stat.Skipped)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundFoo := false
		for dec.More() {
			var m build.StepMetric
			err := dec.Decode(&m)
			if err != nil {
				t.Errorf("decode %v", err)
			}
			if m.StepID == "" {
				continue
			}
			switch filepath.Base(m.Output()) {
			case "foo.o":
				foundFoo = true
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
				if !m.TwoPhaseCacheHit {
					t.Errorf("%s TwoPhaseCacheHit=%t; want true", m.Output(), m.TwoPhaseCacheHit)
				}
			default:
				t.Errorf("unexpected output %q: %#v", m.Output(), m)
			}
		}
		if !foundFoo {
			t.Errorf("foo.o not found in metrics")
		}
	}()

	func() {
		t.Logf("--- third build: update base/foo.h, expect two-phase cache miss and bar.out rebuilt")
		content = []byte("output of foo.o modified\n")
		modifyFile(t, dir, "base/foo.h", func(buf []byte) []byte {
			return append(buf, []byte("\n// modified")...)
		})
		var metricsBuffer syncBuffer
		stat, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 1 { // all(phony)
			t.Errorf("Skipped=%d; want 1", stat.Skipped)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		built := make(map[string]bool)
		for dec.More() {
			var m build.StepMetric
			err := dec.Decode(&m)
			if err != nil {
				t.Errorf("decode %v", err)
			}
			if m.StepID == "" {
				continue
			}
			built[filepath.Base(m.Output())] = true
		}
		if !built["foo.o"] {
			t.Errorf("foo.o was not built")
		}
		if !built["bar.out"] {
			t.Errorf("bar.out was not built")
		}
	}()
}

type recordingTapHelper struct {
	mockTapHelper
	lastEnv []string
}

func (h *recordingTapHelper) Run(ctx context.Context, req *epb.SpawnRequest) (*epb.SpawnResult, error) {
	h.lastEnv = req.GetEnv()
	return h.mockTapHelper.Run(ctx, req)
}

func TestBuild_TwoPhaseCaching_PassEnv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching is available on linux only")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	t.Setenv("TMPDIR", tempDir(t))
	t.Setenv("TMP", tempDir(t))

	build.SetExperimentForTest("two-phase-caching")
	t.Setenv("SOONG_METRICS_AGGREGATION_DIR", "/workspace/out/soong/metrics_aggregation")
	t.Setenv("CIPD_PROXY_URL", "unix:///workspace/out/soong/.temp/cipd123/proxy.unix")
	t.Setenv("RBE_metrics_project", "test-metrics-project")

	content := []byte("output of foo.o\n")
	depContent := []byte("foo.o: ../../base/foo.cc ../../base/foo.h\n")
	var remoteEnvVars []*rpb.Command_EnvironmentVariable
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			cmd := &rpb.Command{}
			if err := fakere.FetchProto(ctx, action.GetCommandDigest(), cmd); err != nil {
				return nil, err
			}
			remoteEnvVars = cmd.GetEnvironmentVariables()
			dg, err := fakere.Put(ctx, content)
			if err != nil {
				return nil, err
			}
			depDg, err := fakere.Put(ctx, depContent)
			if err != nil {
				return nil, err
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "foo.o",
						Digest: dg,
					},
					{
						Path:   "foo.o.d",
						Digest: depDg,
					},
				},
			}, nil
		},
	}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	localCache, err := reapi.NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ds.Client = reapitest.NewWithOption(ctx, t, fakere, reapi.Option{
		LocalCache:                    localCache,
		DisableTwoPhaseCachingMethods: true,
	})
	err = ds.Client.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds.Cache = ds.Client.CacheStore()

	runNinja := func(ctx context.Context, t *testing.T, metricsBuffer *syncBuffer) (build.Stats, error) {
		t.Helper()
		hashfsOpts := hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			DataSource:  ds,
		}
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.Cache,
			EnableRead: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.RECacheEnableWrite = true
		opt.REAPIClient = ds.Client
		opt.REExecEnable = true
		if metricsBuffer != nil {
			opt.MetricsJSONWriter = metricsBuffer
		}
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	tapHelper := &recordingTapHelper{}
	origHelper := localexec.SetSpawnHelper(tapHelper)
	defer localexec.SetSpawnHelper(origHelper)

	envMapFromSlice := func(envs []string) map[string]string {
		m := make(map[string]string, len(envs))
		for _, e := range envs {
			if k, v, ok := strings.Cut(e, "="); ok {
				m[k] = v
			}
		}
		return m
	}

	var firstBarKey, firstBarDigest string
	func() {
		t.Logf("--- first build: remote foo.o has no env in REAPI payload; local bar.out gets env (without excluded RBE_metrics_project) and records normalized env in 2PC cache")
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		if len(remoteEnvVars) != 0 {
			t.Errorf("remote RBE Command EnvironmentVariables = %v; want empty", remoteEnvVars)
		}
		localEnv := envMapFromSlice(tapHelper.lastEnv)
		wantAggDir := "/workspace/out/soong/metrics_aggregation"
		if got := localEnv["SOONG_METRICS_AGGREGATION_DIR"]; got != wantAggDir {
			t.Errorf("local action env[SOONG_METRICS_AGGREGATION_DIR] = %q; want %q", got, wantAggDir)
		}
		wantCIPD := "unix:///workspace/out/soong/.temp/cipd123/proxy.unix"
		if got := localEnv["CIPD_PROXY_URL"]; got != wantCIPD {
			t.Errorf("local action env[CIPD_PROXY_URL] = %q; want %q", got, wantCIPD)
		}
		if _, ok := localEnv["RBE_metrics_project"]; ok {
			t.Errorf("local action env unexpectedly contains RBE_metrics_project=%q", localEnv["RBE_metrics_project"])
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "bar.out" {
				firstBarKey = m.TwoPhaseCachingKey
				firstBarDigest = m.Digest
				if !m.CacheWrite {
					t.Errorf("bar.out CacheWrite=%t; want true", m.CacheWrite)
				}
			}
		}
		if firstBarKey == "" {
			t.Fatalf("firstBarKey is empty")
		}
		if firstBarDigest == "" {
			t.Fatalf("firstBarDigest is empty")
		}
	}()

	func() {
		t.Logf("--- second build: incremental build with normalized relative path and changed omitted CIPD_PROXY_URL should be a no-op")
		t.Setenv("SOONG_METRICS_AGGREGATION_DIR", "out/soong/metrics_aggregation")
		t.Setenv("CIPD_PROXY_URL", "unix:///workspace/out/soong/.temp/cipd999/proxy.unix")
		stats, err := runNinja(ctx, t, nil)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Skipped != stats.Total {
			t.Errorf("stats.Skipped=%d; want %d (no-op incremental build)", stats.Skipped, stats.Total)
		}
	}()

	func() {
		t.Logf("--- third build: clean local bar.out and state, expect two-phase cache hit and same lookup key")
		for _, p := range []string{"bar.out", ".siso_fs_state"} {
			if err := os.RemoveAll(filepath.Join(dir, "out/siso", p)); err != nil {
				t.Fatal(err)
			}
		}
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundBar := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "bar.out" {
				foundBar = true
				if m.TwoPhaseCachingKey != firstBarKey {
					t.Errorf("bar.out TwoPhaseCachingKey=%q; want %q", m.TwoPhaseCachingKey, firstBarKey)
				}
				if !m.TwoPhaseCacheHit {
					t.Errorf("bar.out TwoPhaseCacheHit=%t; want true", m.TwoPhaseCacheHit)
				}
			}
		}
		if !foundBar {
			t.Errorf("bar.out not found in metrics")
		}
	}()

	func() {
		t.Logf("--- fourth build: incremental build with changed non-omitted environment variable invalidates local bar.out (without invalidating remote foo.o)")
		t.Setenv("SOONG_METRICS_AGGREGATION_DIR", "out/soong/metrics_aggregation_v2")
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		localEnv := envMapFromSlice(tapHelper.lastEnv)
		if got := localEnv["SOONG_METRICS_AGGREGATION_DIR"]; got != "out/soong/metrics_aggregation_v2" {
			t.Errorf("local action env[SOONG_METRICS_AGGREGATION_DIR] = %q; want %q", got, "out/soong/metrics_aggregation_v2")
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundBar := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "foo.o" {
				t.Errorf("remote foo.o unexpectedly reran on env change: %+v", m)
			}
			if filepath.Base(m.Output()) == "bar.out" {
				foundBar = true
				if m.TwoPhaseCachingKey == "" || m.TwoPhaseCachingKey == firstBarKey {
					t.Errorf("bar.out TwoPhaseCachingKey=%q; want distinct non-empty key != %q", m.TwoPhaseCachingKey, firstBarKey)
				}
				if m.TwoPhaseCacheHit {
					t.Errorf("bar.out TwoPhaseCacheHit=%t; want false when env changed", m.TwoPhaseCacheHit)
				}
				if m.Digest == "" || m.Digest == firstBarDigest {
					t.Errorf("bar.out Digest=%q; want distinct non-empty digest != %q", m.Digest, firstBarDigest)
				}
			}
		}
		if !foundBar {
			t.Errorf("bar.out not found in metrics")
		}
	}()

	func() {
		t.Logf("--- fifth build: subsequent incremental build with same environment is a no-op")
		stats, err := runNinja(ctx, t, nil)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Skipped != stats.Total {
			t.Errorf("stats.Skipped=%d; want %d (no-op incremental build)", stats.Skipped, stats.Total)
		}
	}()

	func() {
		t.Logf("--- sixth build: incremental build switching back to first environment invalidates bar.out and hits 2PC cache without cleaning state")
		t.Setenv("SOONG_METRICS_AGGREGATION_DIR", "out/soong/metrics_aggregation")
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundBar := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "foo.o" {
				t.Errorf("remote foo.o unexpectedly reran on env change: %+v", m)
			}
			if filepath.Base(m.Output()) == "bar.out" {
				foundBar = true
				if m.TwoPhaseCachingKey != firstBarKey {
					t.Errorf("bar.out TwoPhaseCachingKey=%q; want %q", m.TwoPhaseCachingKey, firstBarKey)
				}
				if !m.TwoPhaseCacheHit {
					t.Errorf("bar.out TwoPhaseCacheHit=%t; want true", m.TwoPhaseCacheHit)
				}
			}
		}
		if !foundBar {
			t.Errorf("bar.out not found in metrics")
		}
	}()
}

// Regression test for b/564415739: empty directory created by a local tapped
// action must be cached as an OutputDirectory and materialized on a two-phase
// cache hit.
func TestBuild_TwoPhaseCaching_EmptyDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching is available on linux only")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	t.Setenv("TMPDIR", tempDir(t))
	t.Setenv("TMP", tempDir(t))

	build.SetExperimentForTest("two-phase-caching")

	fakere := &reapitest.Fake{}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	localCache, err := reapi.NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ds.Client = reapitest.NewWithOption(ctx, t, fakere, reapi.Option{
		LocalCache:                    localCache,
		DisableTwoPhaseCachingMethods: true,
	})
	err = ds.Client.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds.Cache = ds.Client.CacheStore()

	runNinja := func(ctx context.Context, t *testing.T, metricsBuffer *syncBuffer) (build.Stats, error) {
		t.Helper()
		hashfsOpts := hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			DataSource:  ds,
		}
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.Cache,
			EnableRead: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.RECacheEnableWrite = true
		opt.REAPIClient = ds.Client
		opt.REExecEnable = true
		if metricsBuffer != nil {
			opt.MetricsJSONWriter = metricsBuffer
		}
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	origHelper := localexec.SetSpawnHelper(mockTapHelper{})
	defer localexec.SetSpawnHelper(origHelper)

	func() {
		t.Logf("--- first build: local execution with mock spawn helper populates 2PC cache with empty_dir")
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, "out/siso/empty_dir"))
		if err != nil || !fi.IsDir() {
			t.Fatalf("empty_dir stat=%v, err=%v; want directory", fi, err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "foo.out" && !m.CacheWrite {
				t.Errorf("foo.out CacheWrite=%t; want true", m.CacheWrite)
			}
		}
	}()

	func() {
		t.Logf("--- second build: clean out/siso outputs and state, expect 2PC cache hit and empty_dir materialized")
		for _, p := range []string{"foo.out", "bar.out", "empty_dir", ".siso_fs_state"} {
			if err := os.RemoveAll(filepath.Join(dir, "out/siso", p)); err != nil {
				t.Fatal(err)
			}
		}
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, "out/siso/empty_dir"))
		if err != nil || !fi.IsDir() {
			t.Errorf("empty_dir not materialized from 2PC cache: stat=%v, err=%v", fi, err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundFoo := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "foo.out" {
				foundFoo = true
				if !m.TwoPhaseCacheHit {
					t.Errorf("foo.out TwoPhaseCacheHit=%t; want true", m.TwoPhaseCacheHit)
				}
			}
		}
		if !foundFoo {
			t.Errorf("foo.out not found in metrics")
		}
	}()
}

func TestBuild_TwoPhaseCaching_PhonyAndOrderOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching is available on linux only")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	t.Setenv("TMPDIR", tempDir(t))
	t.Setenv("TMP", tempDir(t))

	build.SetExperimentForTest("two-phase-caching,expand-phony-trigger-inputs")

	fakere := &reapitest.Fake{}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	localCache, err := reapi.NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ds.Client = reapitest.NewWithOption(ctx, t, fakere, reapi.Option{
		LocalCache:                    localCache,
		DisableTwoPhaseCachingMethods: true,
	})
	err = ds.Client.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds.Cache = ds.Client.CacheStore()

	runNinja := func(ctx context.Context, t *testing.T, metricsBuffer *syncBuffer) (build.Stats, error) {
		t.Helper()
		hashfsOpts := hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			DataSource:  ds,
		}
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.Cache,
			EnableRead: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.RECacheEnableWrite = true
		opt.REAPIClient = ds.Client
		opt.REExecEnable = true
		if metricsBuffer != nil {
			opt.MetricsJSONWriter = metricsBuffer
		}
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	origHelper := localexec.SetSpawnHelper(mockTapHelper{})
	defer localexec.SetSpawnHelper(origHelper)

	var firstKey string
	func() {
		t.Logf("--- first build: populate 2PC cache with phony trigger input, read order-only input, and unread order-only inputs")
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "foo.out" {
				firstKey = m.TwoPhaseCachingKey
				if !m.CacheWrite {
					t.Errorf("foo.out CacheWrite=%t; want true", m.CacheWrite)
				}
			}
		}
		if firstKey == "" {
			t.Fatalf("firstKey is empty")
		}
	}()

	func() {
		t.Logf("--- second build: modify unread order-only inputs (direct and via phony), expect 2PC cache hit with same lookup key")
		modifyFile(t, dir, "base/unread.in", func(buf []byte) []byte {
			return []byte("unread v2 modified\n")
		})
		modifyFile(t, dir, "base/unread_via_phony.in", func(buf []byte) []byte {
			return []byte("unread via phony v2 modified\n")
		})
		for _, p := range []string{"foo.out", ".siso_fs_state"} {
			if err := os.RemoveAll(filepath.Join(dir, "out/siso", p)); err != nil {
				t.Fatal(err)
			}
		}
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundFoo := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "foo.out" {
				foundFoo = true
				if m.TwoPhaseCachingKey != firstKey {
					t.Errorf("foo.out TwoPhaseCachingKey=%q; want %q", m.TwoPhaseCachingKey, firstKey)
				}
				if !m.TwoPhaseCacheHit {
					t.Errorf("foo.out TwoPhaseCacheHit=%t; want true when only unread order-only inputs changed", m.TwoPhaseCacheHit)
				}
			}
		}
		if !foundFoo {
			t.Errorf("foo.out not found in metrics")
		}
	}()

	func() {
		t.Logf("--- third build: modify read order-only input, expect same Phase 1 lookup key but Phase 2 miss")
		modifyFile(t, dir, "base/read_order_only.in", func(buf []byte) []byte {
			return []byte("read order only v2 modified\n")
		})
		for _, p := range []string{"foo.out", ".siso_fs_state"} {
			if err := os.RemoveAll(filepath.Join(dir, "out/siso", p)); err != nil {
				t.Fatal(err)
			}
		}
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundFoo := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "foo.out" {
				foundFoo = true
				if m.TwoPhaseCachingKey != firstKey {
					t.Errorf("foo.out TwoPhaseCachingKey=%q; want %q", m.TwoPhaseCachingKey, firstKey)
				}
				if m.TwoPhaseCacheHit {
					t.Errorf("foo.out TwoPhaseCacheHit=%t; want false when read order-only input changed", m.TwoPhaseCacheHit)
				}
				if m.TwoPhaseCachingActions != 1 {
					t.Errorf("foo.out TwoPhaseCachingActions=%d; want 1 candidate checked on Phase 2 miss", m.TwoPhaseCachingActions)
				}
			}
		}
		if !foundFoo {
			t.Errorf("foo.out not found in metrics")
		}
	}()

	func() {
		t.Logf("--- fourth build: modify tool behind phony trigger input, expect distinct 2PC lookup key")
		modifyFile(t, dir, "tools/gen.py", func(buf []byte) []byte {
			return append(buf, []byte("\n# tool modified\n")...)
		})
		for _, p := range []string{"foo.out", ".siso_fs_state"} {
			if err := os.RemoveAll(filepath.Join(dir, "out/siso", p)); err != nil {
				t.Fatal(err)
			}
		}
		var metricsBuffer syncBuffer
		_, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundFoo := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("decode %v", err)
			}
			if filepath.Base(m.Output()) == "foo.out" {
				foundFoo = true
				if m.TwoPhaseCachingKey == "" || m.TwoPhaseCachingKey == firstKey {
					t.Errorf("foo.out TwoPhaseCachingKey=%q; want distinct non-empty key != %q", m.TwoPhaseCachingKey, firstKey)
				}
				if m.TwoPhaseCacheHit {
					t.Errorf("foo.out TwoPhaseCacheHit=%t; want false when tool behind phony changed", m.TwoPhaseCacheHit)
				}
				if m.TwoPhaseCachingActions != 0 {
					t.Errorf("foo.out TwoPhaseCachingActions=%d; want 0 for new lookup key", m.TwoPhaseCachingActions)
				}
			}
		}
		if !foundFoo {
			t.Errorf("foo.out not found in metrics")
		}
	}()
}

// Regression test for b/570100436:
//   - Declared outputs that unlink-before-write (reported in both Writes and
//     Deletes) must retain their CmdHash in .siso_fs_state so a subsequent
//     incremental build is a no-op instead of rebuilding due to
//     "command line not found in log".
//   - Undeclared outputs discovered by tap (both overwritten in-place and
//     unlinked-before-write) must be refreshed in HashFS and uploaded to the
//     2PC cache with their latest contents, while temporary files written then
//     deleted during execution must be forgotten and excluded.
func TestBuild_TwoPhaseCaching_TapOutputsAndDeletes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching is available on linux only")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	t.Setenv("TMPDIR", tempDir(t))
	t.Setenv("TMP", tempDir(t))

	build.SetExperimentForTest("two-phase-caching")

	fakere := &reapitest.Fake{}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	localCache, err := reapi.NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ds.Client = reapitest.NewWithOption(ctx, t, fakere, reapi.Option{
		LocalCache:                    localCache,
		DisableTwoPhaseCachingMethods: true,
	})
	err = ds.Client.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds.Cache = ds.Client.CacheStore()

	runNinja := func(ctx context.Context, t *testing.T, metricsBuffer *syncBuffer) (build.Stats, error) {
		t.Helper()
		hashfsOpts := hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			DataSource:  ds,
		}
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.Cache,
			EnableRead: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.RECacheEnableWrite = true
		opt.REAPIClient = ds.Client
		opt.REExecEnable = true
		if metricsBuffer != nil {
			opt.MetricsJSONWriter = metricsBuffer
		}
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	origHelper := localexec.SetSpawnHelper(mockTapHelper{})
	defer localexec.SetSpawnHelper(origHelper)

	checkOutputs := func(want string) error {
		var errs []error
		for _, fname := range []string{"foo.out", "bar.out", "undeclared.out", "undeclared_recreated.out"} {
			got, err := os.ReadFile(filepath.Join(dir, "out/siso", fname))
			if err != nil || string(got) != want {
				errs = append(errs, fmt.Errorf("os.ReadFile(%q) = %q, %v; want %q, nil", fname, string(got), err, want))
			}
		}
		if fi, err := os.Stat(filepath.Join(dir, "out/siso/temp.tmp")); !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("os.Stat(%q) = %v, %v; want nil, %v", "temp.tmp", fi, err, fs.ErrNotExist))
		}
		return errors.Join(errs...)
	}

	func() {
		t.Logf("--- first build: populate .siso_fs_state and 2PC cache")
		var metricsBuffer syncBuffer
		stats, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatalf("runNinja(ctx, t) = %v, %v; want _, nil", stats, err)
		}
		if err := checkOutputs("v1\n"); err != nil {
			t.Errorf("checkOutputs(%q): %v", "v1\n", err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("dec.Decode(&m) = %v; want nil", err)
			}
			if filepath.Base(m.Output()) == "foo.out" && !m.CacheWrite {
				t.Errorf("foo.out CacheWrite = %t; want true", m.CacheWrite)
			}
		}
	}()

	func() {
		t.Logf("--- second build: incremental build must be a no-op (declared output foo.out retains CmdHash despite unlink-before-write)")
		stats, err := runNinja(ctx, t, nil)
		if err != nil {
			t.Fatalf("runNinja(ctx, t) = %v, %v; want _, nil", stats, err)
		}
		if stats.Skipped != stats.Total {
			t.Errorf("runNinja(ctx, t) stats.Skipped = %d; want %d (no-op incremental build)", stats.Skipped, stats.Total)
		}
	}()

	func() {
		t.Logf("--- third build: modify base/foo.in in incremental build so undeclared outputs are overwritten/recreated and re-cached")
		modifyFile(t, dir, "base/foo.in", func(buf []byte) []byte {
			return []byte("v2\n")
		})
		var metricsBuffer syncBuffer
		stats, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatalf("runNinja(ctx, t) = %v, %v; want _, nil", stats, err)
		}
		if err := checkOutputs("v2\n"); err != nil {
			t.Errorf("checkOutputs(%q): %v", "v2\n", err)
		}
	}()

	func() {
		t.Logf("--- fourth build: clean outputs and state, expect 2PC cache hit with updated v2 contents for discovered outputs")
		for _, p := range []string{"foo.out", "bar.out", "undeclared.out", "undeclared_recreated.out", ".siso_fs_state"} {
			if err := os.RemoveAll(filepath.Join(dir, "out/siso", p)); err != nil {
				t.Fatalf("os.RemoveAll(%q) = %v; want nil", p, err)
			}
		}
		var metricsBuffer syncBuffer
		stats, err := runNinja(ctx, t, &metricsBuffer)
		if err != nil {
			t.Fatalf("runNinja(ctx, t) = %v, %v; want _, nil", stats, err)
		}
		if err := checkOutputs("v2\n"); err != nil {
			t.Errorf("checkOutputs(%q): %v", "v2\n", err)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundFoo := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Errorf("dec.Decode(&m) = %v; want nil", err)
			}
			if filepath.Base(m.Output()) == "foo.out" {
				foundFoo = true
				if !m.TwoPhaseCacheHit {
					t.Errorf("foo.out TwoPhaseCacheHit = %t; want true", m.TwoPhaseCacheHit)
				}
			}
		}
		if !foundFoo {
			t.Errorf("foo.out not found in metrics")
		}
	}()
}
