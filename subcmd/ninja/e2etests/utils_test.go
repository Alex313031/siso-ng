// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/buildconfig"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

var (
	testSerial = flag.Bool("siso-test-serial", false, "execute the actual test sequentially in the current process")
)

// runInSubProcess runs the test in a separate process to allow safe chdir.
// It returns true if the test should proceed (i.e., we are in the subprocess).
// It returns false if we are in the parent process and should skip the actual test logic.
func runInSubProcess(t *testing.T) bool {
	t.Helper()
	if *testSerial {
		return true // Execute the actual test in the current process
	}
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("could not get executable: %v", err)
	}

	// -siso-test-serial is the recursion guard; it must precede any forwarded
	// argument, because flag parsing stops at the first non-flag argument and
	// an unparsed guard means infinite re-exec (fork bomb).
	args := []string{"-test.run=^" + t.Name() + "$", "-siso-test-serial"}
	skipNext := false

	// We must selectively forward flags from the parent `go test` runner to the subprocess:
	// 1. Keep safe flags (e.g., -test.v, -test.timeout) to preserve expected user behavior.
	// 2. Drop multiplier flags (e.g., -test.count) to prevent exponential test executions.
	// 3. Rewrite profiling/output flags (e.g., -test.cpuprofile) by appending the test name
	//    so that parallel subprocesses do not concurrently write to and corrupt a single shared file.
	for i, arg := range os.Args[1:] {
		if skipNext {
			skipNext = false
			continue
		}

		if strings.HasPrefix(arg, "-test.") {
			if strings.HasPrefix(arg, "-test.v=") || arg == "-test.v" ||
				strings.HasPrefix(arg, "-test.short=") || arg == "-test.short" ||
				strings.HasPrefix(arg, "-test.failfast=") || arg == "-test.failfast" ||
				strings.HasPrefix(arg, "-test.paniconexit0=") || arg == "-test.paniconexit0" ||
				strings.HasPrefix(arg, "-test.timeout=") || arg == "-test.timeout" {
				args = append(args, arg)
			} else if strings.HasPrefix(arg, "-test.cpuprofile") ||
				strings.HasPrefix(arg, "-test.memprofile") ||
				strings.HasPrefix(arg, "-test.mutexprofile") ||
				strings.HasPrefix(arg, "-test.blockprofile") ||
				strings.HasPrefix(arg, "-test.trace") ||
				strings.HasPrefix(arg, "-test.outputdir") {

				// Handle both `-test.cpuprofile=cpu.prof` and `-test.cpuprofile cpu.prof`
				val := ""
				key := arg
				hasEq := strings.Contains(arg, "=")
				if hasEq {
					parts := strings.SplitN(arg, "=", 2)
					key = parts[0]
					val = parts[1]
				} else if i+1 < len(os.Args[1:]) {
					val = os.Args[1:][i+1]
					skipNext = true
				}

				if val != "" {
					newVal := val + "." + t.Name()
					if key == "-test.outputdir" {
						if err := os.MkdirAll(newVal, 0755); err != nil {
							t.Fatalf("os.MkdirAll(%q)=%v; want nil err", newVal, err)
						}
					}
					args = append(args, key+"="+newVal)
				} else {
					args = append(args, arg)
				}
			}
		} else {
			args = append(args, arg)
		}
	}

	cmd := exec.CommandContext(t.Context(), exe, args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		t.Logf("\n%s", out)
	}
	if err != nil {
		t.Fatalf("subprocess test failed: %v", err)
	}
	return false // Indicates parent should skip test logic
}

// tempDir returns real path of temp dir.
// mac uses /tmp -> private/tmp symlink, so TempDir may contains
// symlink in the path, which confuses hashfs, so use EvalSymlinks
// to make it real path.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func setupFiles(t *testing.T, dir, name string, deletes []string) {
	t.Helper()
	root := filepath.Join("testdata", name)
	err := filepath.Walk(root, func(pathname string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, pathname)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dir, name), 0755)
		}
		if info.Mode()&fs.ModeSymlink == fs.ModeSymlink {
			target, err := os.Readlink(pathname)
			if err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(dir, name))
		}
		buf, err := os.ReadFile(pathname)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), buf, info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range deletes {
		err = os.Remove(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
	}
}

// make sure file at dir/name is modified, i.e. have different mtime.
// gen takes old content and returns new content.
func modifyFile(t *testing.T, dir, name string, gen func([]byte) []byte) {
	t.Helper()
	oldTime := time.Now()
	t.Logf("-- modify %s", name)
	fullname := filepath.Join(dir, name)
	fi, err := os.Stat(fullname)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := os.ReadFile(fullname)
	if err != nil {
		t.Fatal(err)
	}
	buf = gen(buf)
	err = os.WriteFile(fullname, buf, fi.Mode())
	if err != nil {
		t.Fatal(err)
	}
	for {
		err = os.Chtimes(fullname, time.Time{}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		nfi, err := os.Stat(fullname)
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().Equal(nfi.ModTime()) || oldTime.Equal(nfi.ModTime()) {
			time.Sleep(1 * time.Millisecond)
			continue
		}
		t.Logf("-- modified %s %s", name, nfi.ModTime())
		return
	}
}

// like modifyFile, make sure file at dir/name exists and mtime is updated.
func touchFile(t *testing.T, dir, name string) {
	t.Helper()
	t.Logf("-- touch %s", name)
	oldTime := time.Now()
	fullname := filepath.Join(dir, name)
	_, err := os.Stat(fullname)
	if errors.Is(err, fs.ErrNotExist) {
		err = os.WriteFile(fullname, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	for {
		err = os.Chtimes(fullname, time.Time{}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		nfi, err := os.Stat(fullname)
		if err != nil {
			t.Fatal(err)
		}
		// chtimes might not make the file is newer than previous
		// build's artifact.
		// make sure it's newer than any of previous build's
		// artifact, so touch would trigger the step that
		// use the file as input.
		if oldTime.Equal(nfi.ModTime()) {
			time.Sleep(1 * time.Millisecond)
			continue
		}
		t.Logf("-- touched %s %s", name, nfi.ModTime())
		return
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncBuffer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(data)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *syncBuffer) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Bytes()
}

func setupBuild(ctx context.Context, t *testing.T, dir string, fsopt hashfs.Option) (build.Options, *ninjabuild.Graph, func()) {
	t.Helper()
	var cleanups []func()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	// not t.Chdir as it needs to restore current working directory
	// by cleanup (i.e. build finished), not at the end of test.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	})
	err = os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}

	var hashfsSetStateLog syncBuffer
	fsopt.SetStateLogger = &hashfsSetStateLog
	hashFS, err := hashfs.New(ctx, fsopt)
	if err != nil {
		t.Fatal(err)
	}
	err = hashFS.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := hashFS.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s := hashfsSetStateLog.buf.String(); s != "" {
			t.Log(s)
		}
	})
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := build.NewPath(dir, "out/siso")
	depsLog, err := ninjabuild.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := depsLog.Close()
		if err != nil {
			t.Fatal(err)
		}
	})
	stepConfig, err := ninjabuild.NewStepConfig(ctx, config, path, "build.ninja", ".")
	if err != nil {
		t.Fatal(err)
	}
	nstate, err := ninjabuild.Load(ctx, "build.ninja", path)
	if err != nil {
		t.Fatal(err)
	}

	graph := ninjabuild.NewGraph(ctx, "build.ninja", nstate, config, path, hashFS, stepConfig, depsLog)

	cachestore, err := build.NewLocalCache(".siso_cache")
	if err != nil {
		t.Logf("no local cache enabled: %v", err)
	}
	cache, err := build.NewCache(ctx, build.CacheOptions{
		Store: cachestore,
	})
	if err != nil {
		t.Fatal(err)
	}
	var explain syncBuffer
	cleanups = append(cleanups, func() {
		if s := explain.buf.String(); s != "" {
			t.Log(s)
		}
	})
	opt := build.Options{
		Path:            path,
		HashFS:          hashFS,
		REExecEnable:    true,
		Cache:           cache,
		FailuresAllowed: 1,
		Limits:          build.UnitTestLimits(ctx),
		ExplainWriter:   &explain,
	}
	return opt, graph, func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

func openDepsLog(ctx context.Context, t *testing.T, dir string) (*ninjabuild.DepsLog, func()) {
	t.Helper()
	var cleanups []func()

	// not t.Chdir as it needs to restore current working directory
	// by cleanup (i.e. build finished), not at the end of test.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := os.Chdir(wd)
		if err != nil {
			t.Fatal(err)
		}
	})
	err = os.Chdir(filepath.Join(dir, "out/siso"))
	if err != nil {
		t.Fatal(err)
	}

	depsLog, err := ninjabuild.NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	cleanups = append(cleanups, func() {
		err := depsLog.Close()
		if err != nil {
			t.Fatal(err)
		}
	})
	return depsLog, func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}
