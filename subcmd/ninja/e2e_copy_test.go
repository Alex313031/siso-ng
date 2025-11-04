// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
)

func TestBuild_Copy(t *testing.T) {
	ctx := t.Context()

	ninja := func(t *testing.T, dir string, outputLocal hashfs.OutputLocalFunc) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: outputLocal,
		})
		defer cleanup()
		stats, err := runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
		return stats, err
	}

	checkFSStat := func(dir string, m map[string]*pb.Entry, wants []string, notWants []string) error {
		var mismatches []string
		for _, fname := range wants {
			fullname := filepath.ToSlash(filepath.Join(dir, fname))
			_, ok := m[fullname]
			if !ok {
				mismatches = append(mismatches, fmt.Sprintf("missing %q in fs state", fname))
			}
		}
		for _, fname := range notWants {
			fullname := filepath.ToSlash(filepath.Join(dir, fname))
			_, ok := m[fullname]
			if ok {
				mismatches = append(mismatches, fmt.Sprintf("exists %q in fs state", fname))
			}
		}
		if len(mismatches) > 0 {
			var files []string
			for k := range m {
				files = append(files, k)
			}
			sort.Strings(files)
			return fmt.Errorf("unexpected fs state: %s\nfs state=%#v", mismatches, files)
		}
		return nil
	}

	checkDisk := func(dir string, files []string, wantErr error) error {
		var mismatches []string
		for _, fname := range files {
			fullname := filepath.Join(dir, fname)
			_, err := os.Stat(fullname)
			if !errors.Is(err, wantErr) {
				mismatches = append(mismatches, fmt.Sprintf("%q on disk %v; want %v", fname, err, wantErr))
			}
		}
		if len(mismatches) > 0 {
			return fmt.Errorf("unexpected disk %s", mismatches)
		}
		return nil
	}
	// need to use this test name, not subtest name in subtests below.
	tname := t.Name()

	for _, tc := range []struct {
		name        string
		outputLocal hashfs.OutputLocalFunc
		onDisk      error
	}{
		{
			name:        "minimum",
			outputLocal: func(context.Context, string) bool { return false },
			onDisk:      fs.ErrNotExist,
		},
		{
			name:        "full",
			outputLocal: func(context.Context, string) bool { return true },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tempDir(t)
			setupFiles(t, dir, tname, nil)
			stats, err := ninja(t, dir, tc.outputLocal)
			if err != nil {
				t.Fatalf("ninja %v; want nil err", err)
			}
			if stats.NoExec != 2 {
				t.Errorf("stats.NoExec=%d want=2\n%#v", stats.NoExec, stats)
			}
			st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
			if err != nil {
				t.Errorf("hashfs.Load=%v; want nil err", err)
			}
			m := hashfs.StateMap(st)

			wantFiles := []string{
				"out/siso/gen/cache/info.txt",
				"out/siso/gen/cache/data/data.txt",
				"out/siso/gen/cache/data/subdir/subdir.txt",
				"out/siso/gen/file",
			}
			err = checkFSStat(dir, m, wantFiles, nil)
			if err != nil {
				t.Error(err)
			}
			// handler outputs are always written on the disk.
			err = checkDisk(dir, []string{"out/siso/gen/file"}, nil)
			if err != nil {
				t.Error(err)
			}
			err = checkDisk(dir, wantFiles[:3], tc.onDisk)
			if err != nil {
				t.Error(err)
			}
			t.Logf("remove cache/info.txt and check copy remove info.txt in dst")
			time.Sleep(300 * time.Millisecond)
			err = os.Remove(filepath.Join(dir, "cache/info.txt"))
			if err != nil {
				t.Fatal(err)
			}

			stats, err = ninja(t, dir, tc.outputLocal)
			if err != nil {
				t.Fatalf("ninja %v; want nil err", err)
			}
			if stats.NoExec != 1 {
				t.Errorf("stats.NoExec=%d want=1\n%#v", stats.NoExec, stats)
			}
			st, err = hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
			if err != nil {
				t.Errorf("hashfs.Load=%v; want nil err", err)
			}
			m = hashfs.StateMap(st)

			wantFiles = []string{
				"out/siso/gen/cache/data/data.txt",
				"out/siso/gen/cache/data/subdir/subdir.txt",
				"out/siso/gen/file",
			}
			err = checkFSStat(dir, m, wantFiles, []string{"out/siso/gen/cache/info.txt"})
			if err != nil {
				t.Error(err)
			}
			// handler outputs are always written on the disk.
			err = checkDisk(dir, []string{"out/siso/gen/file"}, nil)
			if err != nil {
				t.Error(err)
			}
			err = checkDisk(dir, []string{"out/siso/gen/cache/info.txt"}, fs.ErrNotExist)
			if err != nil {
				t.Error(err)
			}
			err = checkDisk(dir, wantFiles[:2], tc.onDisk)
			if err != nil {
				t.Error(err)
			}
		})
	}
}

func TestBuild_CopyLocalOut(t *testing.T) {
	ctx := t.Context()

	ninja := func(t *testing.T, dir string) error {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		_, err := runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
		return err
	}

	version := func(s []byte) string {
		i := bytes.Index(s, []byte("version "))
		if i < 0 {
			return "<no version>"
		}
		v := s[i+len("version "):]
		i = bytes.IndexAny(v, "\r\n")
		if i < 0 {
			return string(bytes.TrimSpace(v))
		}
		return string(bytes.TrimSpace(v[:i]))
	}

	dir := tempDir(t)
	setupFiles(t, dir, t.Name(), nil)
	err := ninja(t, dir)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "snapshot.in"))
	if err != nil {
		t.Fatal(err)
	}
	if version(src) != "1.2.3" {
		t.Fatalf("unexpected version %q", version(src))
	}
	out, err := os.ReadFile(filepath.Join(dir, "out/siso/Foo Framework.framework/Versions/C/Resource/snapshot.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := version(out), version(src); got != want {
		t.Errorf("snapshot version mismatch: got=%q want=%q", got, want)
	}

	t.Logf("-- update snapshot.in")
	newSrc := bytes.Replace(src, []byte("1.2.3"), []byte("1.2.4"), 1)
	if bytes.Equal(src, newSrc) {
		t.Fatalf("no change in snapshot.in? old=%q new=%q", src, newSrc)
	}
	if version(newSrc) != "1.2.4" {
		t.Fatalf("unexpected version %q", version(newSrc))
	}
	modifyFile(t, dir, "snapshot.in", func([]byte) []byte {
		return newSrc
	})

	err = ninja(t, dir)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	out, err = os.ReadFile(filepath.Join(dir, "out/siso/Foo Framework.framework/Versions/C/Resource/snapshot.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := version(out), version(newSrc); got != want {
		t.Errorf("snapshot version mismatch: got=%q want=%q", got, want)
	}
}

func TestBuild_CopyBundleDataRemovedFile(t *testing.T) {
	ctx := t.Context()

	ninja := func(t *testing.T, dir string) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		stats, err := runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
		return stats, err
	}

	dir := tempDir(t)
	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t, dir)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != 3 || stats.Local != 1 || stats.NoExec != 1 || stats.Total != 3 {
		t.Errorf("stats done=%d local=%d noexec=%d total=%d; want done=3 local=1 noexec=1 total=3; %#v", stats.Done, stats.Local, stats.NoExec, stats.Total, stats)
	}
	_, err = os.Stat(filepath.Join(dir, "out/siso/appex/SSOConfig.plist"))
	if err != nil {
		t.Errorf("out/siso/appex/SSOConfig.plist=%v; want nil err", err)
	}
	_, err = os.Stat(filepath.Join(dir, "out/siso/plugins/appex/SSOConfig.plist"))
	if err != nil {
		t.Errorf("out/siso/plugins/appex/SSOConfig.plist=%v; want nil err", err)
	}

	t.Logf("-- remove SSOConfig.plist")
	err = os.Remove(filepath.Join(dir, "appex/SSOConfig.plist"))
	if err != nil {
		t.Fatal(err)
	}
	touchFile(t, dir, "appex/input")

	t.Logf("-- second build")
	stats, err = ninja(t, dir)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != 3 || stats.Local != 1 || stats.NoExec != 1 || stats.Total != 3 {
		t.Errorf("stats done=%d local=%d noexec=%d total=%d; want done=3 local=1 noexec=1 total=3; %#v", stats.Done, stats.Local, stats.NoExec, stats.Total, stats)
	}
	_, err = os.Stat(filepath.Join(dir, "out/siso/appex/SSOConfig.plist"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("out/siso/appex/SSOConfig.plist=%v; want %v", err, fs.ErrNotExist)
	}
	_, err = os.Stat(filepath.Join(dir, "out/siso/plugins/appex/SSOConfig.plist"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("out/siso/plugins/appex/SSOConfig.plist=%v; want %v", err, fs.ErrNotExist)
	}

}
