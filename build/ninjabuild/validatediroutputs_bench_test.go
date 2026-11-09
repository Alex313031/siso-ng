// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.chromium.org/build/siso/build"
)

// Benchmark validateDirOutputs/Load against a real build.ninja. Skipped unless
// SISO_BENCH_NINJA_DIR is set:
//
//	SISO_BENCH_NINJA_DIR=~/chromium/src/out/Default \
//	  go test ./build/ninjabuild/ -run='^$' -bench=Chromium -benchtime=20x
//
// Chromium has no dir outputs, so a few are injected deep under busy real dirs
// for a realistic trie walk.

// benchNinjaDir returns the absolute SISO_BENCH_NINJA_DIR, or skips.
func benchNinjaDir(b *testing.B) string {
	b.Helper()
	dir := os.Getenv("SISO_BENCH_NINJA_DIR")
	if dir == "" {
		b.Skip("set SISO_BENCH_NINJA_DIR to an output dir with a build.ninja")
	}
	if _, err := os.Stat(filepath.Join(dir, "build.ninja")); err != nil {
		b.Skipf("no build.ninja under %s: %v", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		b.Fatal(err)
	}
	return abs
}

// realOutputDirs returns the n busiest real output dirs (deep, realistic homes
// for a dir output). Caller must be chdir'd to dir.
func realOutputDirs(b *testing.B, ctx context.Context, dir string, n int) []string {
	b.Helper()
	state, err := Load(ctx, "build.ninja", buildPathFor(dir))
	if err != nil {
		b.Fatal(err)
	}
	counts := map[string]int{}
	for id := range state.NumNodes() {
		nd, ok := state.LookupNode(id)
		if !ok {
			continue
		}
		if _, ok := nd.InEdge(); !ok {
			continue // only outputs
		}
		d := path.Dir(nd.Path())
		if d == "." || !filepath.IsLocal(d) {
			continue // not deep in-tree
		}
		counts[d]++
	}
	dirs := make([]string, 0, len(counts))
	for d := range counts {
		dirs = append(dirs, d)
	}
	if len(dirs) < n {
		b.Fatalf("only %d in-tree output directories, need %d", len(dirs), n)
	}
	// Busiest first: more outputs share the prefix, deeper trie walks.
	sort.Slice(dirs, func(i, j int) bool {
		if counts[dirs[i]] != counts[dirs[j]] {
			return counts[dirs[i]] > counts[dirs[j]]
		}
		return dirs[i] < dirs[j]
	})
	dirs = dirs[:n]
	b.Logf("injecting under %d dirs, busiest %q (%d outputs, depth %d)",
		n, dirs[0], counts[dirs[0]], strings.Count(dirs[0], "/")+1)
	return dirs
}

// injectDirOutputs writes a ninja file (in dir, so its subninja resolves) that
// subninjas the real graph and adds a dir output under each hostDir: a fresh
// subdir with nothing real under it, so the scan runs without rejecting.
func injectDirOutputs(b *testing.B, dir string, hostDirs []string) string {
	b.Helper()
	var sb strings.Builder
	sb.WriteString("rule siso_bench_dirout\n  command = true\n")
	for _, h := range hostDirs {
		fmt.Fprintf(&sb, "build %s/siso_bench_dirout/: siso_bench_dirout\n", h)
	}
	sb.WriteString("subninja build.ninja\n")
	f, err := os.CreateTemp(dir, "siso_bench_*.ninja")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { os.Remove(f.Name()) })
	if _, err := f.WriteString(sb.String()); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return filepath.Base(f.Name())
}

// buildPathFor splits an out dir like .../src/out/Default into root + build dir.
func buildPathFor(dir string) *build.Path {
	base := filepath.Join(filepath.Base(filepath.Dir(dir)), filepath.Base(dir))
	root := filepath.Dir(filepath.Dir(dir))
	return build.NewPath(root, base)
}

// BenchmarkValidateDirOutputsChromium times the scan, swept by dir-output count.
func BenchmarkValidateDirOutputsChromium(b *testing.B) {
	dir := benchNinjaDir(b)
	ctx := b.Context()
	b.Chdir(dir)
	hosts := realOutputDirs(b, ctx, dir, 64)
	for _, nDirs := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("dirs=%d", nDirs), func(b *testing.B) {
			fname := injectDirOutputs(b, dir, hosts[:nDirs])
			state, err := Load(ctx, fname, buildPathFor(dir))
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				if err := validateDirOutputs(state); err != nil {
					b.Fatal(err)
				}
			}
			// ReportMetric after ResetTimer, which clears earlier metrics.
			b.ReportMetric(float64(state.NumNodes()), "nodes")
		})
	}
}

// BenchmarkLoadChromium times full Load, to frame the validateDirOutputs cost.
func BenchmarkLoadChromium(b *testing.B) {
	dir := benchNinjaDir(b)
	ctx := b.Context()
	b.Chdir(dir)
	hosts := realOutputDirs(b, ctx, dir, 8)
	fname := injectDirOutputs(b, dir, hosts)
	bp := buildPathFor(dir)
	for b.Loop() {
		if _, err := Load(ctx, fname, bp); err != nil {
			b.Fatal(err)
		}
	}
}

// benchAndroidDir returns the absolute Android top dir and relative combined
// ninja manifest path (e.g. "out/combined-<product>.ninja"), or skips.
//
//	SISO_BENCH_ANDROID_DIR=~/src/android-main \
//	  go test ./build/ninjabuild/ -run='^$' -bench=BenchmarkLoadAndroid -benchmem
func benchAndroidDir(b *testing.B) (dir, fname string) {
	b.Helper()
	dir = os.Getenv("SISO_BENCH_ANDROID_DIR")
	if dir == "" {
		dir = os.Getenv("SISO_BENCH_NINJA_DIR")
	}
	if dir == "" {
		b.Skip("set SISO_BENCH_ANDROID_DIR to an Android checkout with out/combined-*.ninja")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		b.Fatal(err)
	}
	fname = os.Getenv("SISO_BENCH_ANDROID_NINJA")
	if fname != "" {
		if _, err := os.Stat(filepath.Join(abs, fname)); err != nil {
			b.Skipf("no %s under %s: %v", fname, abs, err)
		}
		return abs, fname
	}
	pattern := filepath.Join(abs, AndroidOutDir(), "combined-*.ninja")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		b.Fatal(err)
	}
	if len(matches) == 0 {
		b.Skipf("no %s found", pattern)
	}
	var newest string
	var newestTime int64
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if newest == "" || fi.ModTime().UnixNano() > newestTime {
			newest = m
			newestTime = fi.ModTime().UnixNano()
		}
	}
	if newest == "" {
		b.Skipf("no stat-able %s found", pattern)
	}
	rel, err := filepath.Rel(abs, newest)
	if err != nil {
		b.Fatal(err)
	}
	return abs, filepath.ToSlash(rel)
}

// BenchmarkLoadAndroid times full Load on an Android combined-*.ninja graph,
// matching how build/soong/ui/build/ninja.go invokes siso from the workspace
// root with -f out/combined-<product>.ninja.
func BenchmarkLoadAndroid(b *testing.B) {
	dir, fname := benchAndroidDir(b)
	ctx := b.Context()
	b.Chdir(dir)
	bp := build.NewPath(dir, ".")
	b.Logf("loading %s in %s", fname, dir)
	var numNodes int
	for b.Loop() {
		state, err := Load(ctx, fname, bp)
		if err != nil {
			b.Fatal(err)
		}
		numNodes = state.NumNodes()
	}
	b.ReportMetric(float64(numNodes), "nodes")
}
