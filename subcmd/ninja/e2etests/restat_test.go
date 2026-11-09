// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// Test restat=1 behavior.
// It will skip following steps if mtime is not updated.
// https://ninja-build.org/manual.html#:~:text=appears%20in%20commands.-,restat,-if%20present%2C%20causes
//
// restat
//
//	if present, causes Ninja to re-stat the command’s outputs after
//	execution of the command. Each output whose modification time the
//	command did not change will be treated as though it had never
//	needed to be built. This may cause the output’s reverse
//	dependencies to be removed from the list of pending build
//	actions.
func TestBuild_Restat(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	exists := func(fname string) error {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		return err
	}

	hashfsOpts := hashfs.Option{
		StateFile: ".siso_fs_state",
	}

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()

		_, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := exists("foo.out"); err != nil {
			t.Errorf("foo.out doesn't exist: %v", err)
		}
		if err := exists("bar.out"); err != nil {
			t.Errorf("bar.out doesn't exist: %v", err)
		}
	}()

	func() {
		t.Logf("second build. touch base/foo.in, expect only foo.out is built")
		touchFile(t, dir, "base/foo.in")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 2 { // all(phony) and bar.out
			t.Errorf("Skipped=%d; want 2", stat.Skipped)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
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
			case "foo.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
			default:
				t.Errorf("unexpected output %q: %#v", m.Output(), m)
			}
		}
	}()

	func() {
		t.Logf("third build, update base/foo.in")
		modifyFile(t, dir, "base/foo.in", func(buf []byte) []byte {
			return append(buf, []byte(" modified")...)
		})
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 1 { // all(phony)
			t.Errorf("Skipped=%d; want 1", stat.Skipped)
			for _, fname := range []string{
				"base/foo.in",
				"base/bar.in",
				"out/siso/foo.out",
				"out/siso/bar.out",
			} {
				fi, err := opt.HashFS.Stat(ctx, dir, path.New(fname))
				if err != nil {
					t.Logf("%s: err=%v", fname, err)
				} else {
					t.Logf("%s: %s", fname, fi.ModTime())
				}
			}
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
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
			case "foo.out", "bar.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
			default:
				t.Errorf("unexpected output %q", m.Output())
			}
		}
	}()
}

func TestBuild_Restat_Nsjail(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	skipUnlessNsjailUsable(t)
	ctx := t.Context()
	dir := tempDir(t)

	// allow python3 on luci builder.
	if _, err := os.Stat("/b/s/w/ir"); err == nil {
		// /b/s/w/ir/cipd_bin_packages/cpython3/bin/python3
		t.Setenv("SISO_NSJAIL_PUBLIC_DIRS", "/b/s/w/ir")
	}

	exists := func(fname string) error {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		return err
	}

	hashfsOpts := hashfs.Option{
		StateFile: ".siso_fs_state",
	}

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()

		_, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := exists("foo.out"); err != nil {
			t.Errorf("foo.out doesn't exist: %v", err)
		}
		if err := exists("bar.out"); err != nil {
			t.Errorf("bar.out doesn't exist: %v", err)
		}
	}()

	func() {
		t.Logf("second build. touch base/foo.in, expect only foo.out is built")
		touchFile(t, dir, "base/foo.in")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 2 { // all(phony) and bar.out
			t.Errorf("Skipped=%d; want 2", stat.Skipped)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
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
			case "foo.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
			default:
				t.Errorf("unexpected output %q: %#v", m.Output(), m)
			}
		}
	}()

	func() {
		t.Logf("third build, update base/foo.in")
		modifyFile(t, dir, "base/foo.in", func(buf []byte) []byte {
			return append(buf, []byte(" modified")...)
		})
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 1 { // all(phony)
			t.Errorf("Skipped=%d; want 1", stat.Skipped)
			for _, fname := range []string{
				"base/foo.in",
				"base/bar.in",
				"out/siso/foo.out",
				"out/siso/bar.out",
			} {
				fi, err := opt.HashFS.Stat(ctx, dir, path.New(fname))
				if err != nil {
					t.Logf("%s: err=%v", fname, err)
				} else {
					t.Logf("%s: %s", fname, fi.ModTime())
				}
			}
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
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
			case "foo.out", "bar.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
			default:
				t.Errorf("unexpected output %q", m.Output())
			}
		}
	}()
}

func TestBuild_Restat_NsjailRecreateFile(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	skipUnlessNsjailUsable(t)
	ctx := t.Context()
	dir := tempDir(t)

	// allow python3 on luci builder.
	if _, err := os.Stat("/b/s/w/ir"); err == nil {
		// /b/s/w/ir/cipd_bin_packages/cpython3/bin/python3
		t.Setenv("SISO_NSJAIL_PUBLIC_DIRS", "/b/s/w/ir")
	}

	exists := func(fname string) error {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		return err
	}

	hashfsOpts := hashfs.Option{
		StateFile: ".siso_fs_state",
	}

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()

		_, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := exists("foo.out"); err != nil {
			t.Errorf("foo.out doesn't exist: %v", err)
		}
		if err := exists("bar.out"); err != nil {
			t.Errorf("bar.out doesn't exist: %v", err)
		}
	}()

	func() {
		t.Logf("second build. touch base/foo.in, expect only foo.out is built")
		touchFile(t, dir, "base/foo.in")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 2 { // all(phony) and bar.out
			t.Errorf("Skipped=%d; want 2", stat.Skipped)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
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
			case "foo.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
			default:
				t.Errorf("unexpected output %q: %#v", m.Output(), m)
			}
		}
	}()

	func() {
		t.Logf("third build, update base/foo.in")
		modifyFile(t, dir, "base/foo.in", func(buf []byte) []byte {
			return append(buf, []byte(" modified")...)
		})
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 1 { // all(phony)
			t.Errorf("Skipped=%d; want 1", stat.Skipped)
			for _, fname := range []string{
				"base/foo.in",
				"base/bar.in",
				"out/siso/foo.out",
				"out/siso/bar.out",
			} {
				fi, err := opt.HashFS.Stat(ctx, dir, path.New(fname))
				if err != nil {
					t.Logf("%s: err=%v", fname, err)
				} else {
					t.Logf("%s: %s", fname, fi.ModTime())
				}
			}
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
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
			case "foo.out", "bar.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
			default:
				t.Errorf("unexpected output %q", m.Output())
			}
		}
	}()
}

// Test restat=1 behavior when restat_content=true is set
func TestBuild_Restat_RestatContent(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	exists := func(fname string) error {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		return err
	}

	hashfsOpts := hashfs.Option{
		StateFile: ".siso_fs_state",
	}

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()

		_, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := exists("foo.out"); err != nil {
			t.Errorf("foo.out doesn't exist: %v", err)
		}
		if err := exists("bar.out"); err != nil {
			t.Errorf("bar.out doesn't exist: %v", err)
		}
	}()

	func() {
		t.Logf("second build. touch base/foo.in, expect only foo.out is built")
		touchFile(t, dir, "base/foo.in")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 2 { // all(phony) and bar.out
			t.Errorf("Skipped=%d; want 2", stat.Skipped)
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
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
			case "foo.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
			default:
				t.Errorf("unexpected output %q: %#v", m.Output(), m)
			}
		}
	}()

	func() {
		t.Logf("third build, update base/foo.in")
		modifyFile(t, dir, "base/foo.in", func(buf []byte) []byte {
			return append(buf, []byte(" modified")...)
		})
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 1 { // all(phony)
			t.Errorf("Skipped=%d; want 1", stat.Skipped)
			for _, fname := range []string{
				"base/foo.in",
				"base/bar.in",
				"out/siso/foo.out",
				"out/siso/bar.out",
			} {
				fi, err := opt.HashFS.Stat(ctx, dir, path.New(fname))
				if err != nil {
					t.Logf("%s: err=%v", fname, err)
				} else {
					t.Logf("%s: mod=%s updated=%s", fname, fi.ModTime(), fi.UpdatedTime())
				}
			}
		}
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
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
			case "foo.out", "bar.out":
				if m.Err {
					t.Errorf("%s err=%t; want false", m.Output(), m.Err)
				}
			default:
				t.Errorf("unexpected output %q", m.Output())
			}
		}
	}()
}

// Test the auto-restat-content experiment: restat=1 is treated as
// restat_content for remotely produced outputs, unless restat_content=false
// is set explicitly.
func TestBuild_Restat_AutoRestatContent(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	// fakere emulates tools/action.py: writes the concatenated inputs
	// to --output.
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			cmd := &rpb.Command{}
			err := fakere.FetchProto(ctx, action.CommandDigest, cmd)
			if err != nil {
				return nil, err
			}
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			var output string
			var data []byte
			for _, arg := range cmd.Arguments[2:] {
				if out, ok := strings.CutPrefix(arg, "--output="); ok {
					output = out
					continue
				}
				fn, err := tree.LookupFileNode(ctx, filepath.ToSlash(filepath.Join(cmd.WorkingDirectory, arg)))
				if err != nil {
					return nil, err
				}
				if fn.Digest.SizeBytes == 0 {
					continue
				}
				buf, err := fakere.Fetch(ctx, fn.Digest)
				if err != nil {
					return nil, err
				}
				data = append(data, buf...)
			}
			d, err := fakere.Put(ctx, data)
			if err != nil {
				return nil, err
			}
			return &rpb.ActionResult{
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   output,
						Digest: d,
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
	ds.Client = reapitest.New(ctx, t, fakere)
	ds.Cache = ds.Client.CacheStore()

	runNinja := func(t *testing.T) (build.Stats, []string) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stats, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		var outs []string
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		for dec.More() {
			var m build.StepMetric
			err := dec.Decode(&m)
			if err != nil {
				t.Errorf("decode %v", err)
			}
			if m.StepID == "" {
				continue
			}
			if m.Err {
				t.Errorf("%s err=%t; want false", m.Output(), m.Err)
			}
			outs = append(outs, filepath.Base(m.Output()))
		}
		sort.Strings(outs)
		return stats, outs
	}

	t.Cleanup(func() { build.SetExperimentForTest("") })

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, outs := runNinja(t)
	if want := []string{"bar.out", "baz.out", "foo.out", "qux.out"}; !slices.Equal(outs, want) {
		t.Errorf("first build ran %q; want %q", outs, want)
	}
	if stats.Remote+stats.CacheHit != 4 {
		t.Errorf("remote=%d cache_hit=%d; want 4 remote steps", stats.Remote, stats.CacheHit)
	}

	t.Logf("-- experiment off: touch base/foo.in and base/baz.in, expect all steps to run")
	touchFile(t, dir, "base/foo.in")
	touchFile(t, dir, "base/baz.in")
	_, outs = runNinja(t)
	if want := []string{"bar.out", "baz.out", "foo.out", "qux.out"}; !slices.Equal(outs, want) {
		t.Errorf("experiment off: ran %q; want %q", outs, want)
	}

	build.SetExperimentForTest("auto-restat-content")
	t.Logf("-- experiment on: touch base/foo.in and base/baz.in, expect bar.out to be skipped")
	touchFile(t, dir, "base/foo.in")
	touchFile(t, dir, "base/baz.in")
	stats, outs = runNinja(t)
	// baz.out sets restat_content = false, so qux.out still runs.
	if want := []string{"baz.out", "foo.out", "qux.out"}; !slices.Equal(outs, want) {
		t.Errorf("experiment on: ran %q; want %q", outs, want)
	}
	if stats.Skipped != 2 { // all(phony) and bar.out
		t.Errorf("Skipped=%d; want 2", stats.Skipped)
	}

	t.Logf("-- experiment on: modify base/foo.in, expect foo.out and bar.out to run")
	modifyFile(t, dir, "base/foo.in", func(buf []byte) []byte {
		return append(buf, []byte(" modified")...)
	})
	_, outs = runNinja(t)
	if want := []string{"bar.out", "foo.out"}; !slices.Equal(outs, want) {
		t.Errorf("experiment on, modified: ran %q; want %q", outs, want)
	}
}

// Test restat=1 behavior for multiple output.
// some output may keep mtime, but some output was updated.
func TestBuild_RestatMultiout(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	exists := func(fname string) error {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		return err
	}

	hashfsOpts := hashfs.Option{
		StateFile: ".siso_fs_state",
	}

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()

		_, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := exists("foo.out"); err != nil {
			t.Errorf("foo.out doesn't exist: %v", err)
		}
		if err := exists("foo.out2"); err != nil {
			t.Errorf("foo.out2 doesn't exist: %v", err)
		}
		if err := exists("bar.out"); err != nil {
			t.Errorf("bar.out doesn't exist: %v", err)
		}
	}()

	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
	if err != nil {
		t.Fatal(err)
	}
	stmap := hashfs.StateMap(digest.SHA256, st)

	touchFile(t, dir, "base/foo.in")

	func() {
		t.Logf("second build. touch base/foo.in, expect only foo.out2 is updated and bar.out is updated")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		stat, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if stat.Skipped != 1 { // all(phony)
			t.Errorf("Skipped=%d; want 1", stat.Skipped)
		}
	}()

	nst, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
	if err != nil {
		t.Fatal(err)
	}
	nstmap := hashfs.StateMap(digest.SHA256, nst)

	fname := filepath.ToSlash(filepath.Join(dir, "out/siso/foo.out"))
	first := stmap[fname]
	second := nstmap[fname]
	if x, y := first.GetId().GetModTime(), second.GetId().GetModTime(); x == y {
		t.Errorf("foo.out mtime %d; want not %d", x, y)
	}
	if x, y := first.GetUpdatedTime(), second.GetUpdatedTime(); x >= y {
		t.Errorf("foo.out updated time %d; want < %d", x, y)
	}
	fname = filepath.ToSlash(filepath.Join(dir, "out/siso/foo.out2"))
	first = stmap[fname]
	second = nstmap[fname]
	if x, y := first.GetId().GetModTime(), second.GetId().GetModTime(); x >= y {
		t.Errorf("foo.out2 mtime %d; want < %d", x, y)
	}
	if x, y := first.GetUpdatedTime(), second.GetUpdatedTime(); x >= y {
		t.Errorf("foo.out2 updated time %d; want < %d", x, y)
	}
	fname = filepath.ToSlash(filepath.Join(dir, "out/siso/bar.out"))
	first = stmap[fname]
	second = nstmap[fname]
	if x, y := first.GetId().GetModTime(), second.GetId().GetModTime(); x >= y {
		t.Errorf("bar.out mtime %d; want < %d", x, y)
	}
	if x, y := first.GetUpdatedTime(), second.GetUpdatedTime(); x >= y {
		t.Errorf("bar.out updated time %d; want < %d", x, y)
	}
}
