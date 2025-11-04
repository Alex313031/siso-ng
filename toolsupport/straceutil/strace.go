// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package straceutil provides utilities for strace.
package straceutil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
)

var once sync.Once
var path string

// Available returns whether strace is available or not.
func Available(ctx context.Context) bool {
	once.Do(func() {
		if runtime.GOOS == "windows" {
			// strace exists in msys, but we don't use this
			return
		}
		var err error
		path, err = exec.LookPath("strace")
		if err != nil {
			clog.Warningf(ctx, "strace is not found: %v", err)
			return
		}
	})
	return path != ""
}

// Strace represents a cmd traced by strace.
type Strace struct {
	id   string
	args []string
	dir  string

	// fname is filename of strace output file.
	fname string
}

// New creates a new Strace for cmd.
// It will be fatal error when not available, so check Available before New.
func New(ctx context.Context, id string, args []string, dir string) *Strace {
	if !Available(ctx) {
		clog.Fatalf(ctx, "straceutil.New is called when !Available")
	}
	fname := filepath.Join(os.TempDir(), fmt.Sprintf("%s.trace", id))
	return &Strace{
		id:    id,
		args:  args,
		dir:   dir,
		fname: fname,
	}
}

// Close closes the strace cmd.
func (s *Strace) Close(ctx context.Context) {
	err := os.Remove(s.fname)
	if err != nil {
		clog.Warningf(ctx, "failed to remove %s: %v", s.fname, err)
	}
}

// Args returns args to run under strace.
func (s *Strace) Args(ctx context.Context) []string {
	args := []string{
		path,
		"-f",
		"-e", "trace=file",
		// TODO(b/249633204): "--successful-only" is not available in old strace (4.2 on ubuntu-18.10).
		"-o", s.fname,
	}
	// TODO: use --seccomp-bpf ?
	args = append(args, s.args...)
	return args
}

// PostProcess processes strace outputs and returns inputs/outputs accessed by the cmd.
// inputs/outputs will be absolute paths or relatives to the working directory of the cmd.
func (s *Strace) PostProcess(ctx context.Context) (inputs, outputs []string, err error) {
	b, err := os.ReadFile(s.fname)
	if err != nil {
		return nil, nil, err
	}
	if log.V(3) {
		clog.Infof(ctx, "strace for %s\n%s", s.id, b)
	}
	inputs, outputs = scanStraceData(ctx, b)
	for i := 0; i < len(inputs); i++ {
		target, err := os.Readlink(filepath.Join(s.dir, inputs[i]))
		if err == nil {
			clog.Infof(ctx, "add symlink from %s -> %s", inputs[i], target)
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(inputs[i]), target)
			}
			inputs = append(inputs, target)
		}
	}
	sort.Strings(inputs)
	sort.Strings(outputs)
	return inputs, outputs, nil
}

func scanStraceData(ctx context.Context, buf []byte) ([]string, []string) {
	var inputs []string
	var outputs []string
	iseen := make(map[string]bool)
	oseen := make(map[string]bool)
	for len(buf) > 0 {
		var line []byte
		line, buf = nextLine(buf)
		syscall, fnames, wr := parseTraceLine(ctx, line)
		if log.V(2) {
			clog.Infof(ctx, "trace %q %q wr:%t", syscall, fnames, wr)
		}
		if len(fnames) == 0 {
			continue
		}
		if fnames[0] == "" {
			continue
		}
		if fnames[0] == "." {
			continue
		}
		if strings.HasPrefix(fnames[0], "/proc/") {
			continue
		}
		if strings.HasPrefix(fnames[0], "/dev/") {
			continue
		}
		if !wr {
			if iseen[fnames[0]] || oseen[fnames[0]] {
				continue
			}
			inputs = append(inputs, fnames[0])
			iseen[fnames[0]] = true
			continue
		}
		switch syscall {
		case "rename", "renameat":
			var newoutputs []string
			for _, out := range outputs {
				if out == fnames[0] {
					continue
				}
				newoutputs = append(newoutputs, out)
			}
			outputs = newoutputs
			if !oseen[fnames[1]] {
				outputs = append(outputs, fnames[1])
				oseen[fnames[1]] = true
			}
			for _, fname := range fnames {
				if iseen[fname] {
					var newinputs []string
					for _, in := range inputs {
						if in == fname {
							continue
						}
						newinputs = append(newinputs, in)
					}
					inputs = newinputs
				}
			}
			continue
		case "linkat":
			if !iseen[fnames[0]] {
				inputs = append(inputs, fnames[0])
				iseen[fnames[0]] = true
			}
			if !oseen[fnames[1]] {
				outputs = append(outputs, fnames[1])
				oseen[fnames[1]] = true
			}
			if iseen[fnames[1]] {
				var newinputs []string
				for _, in := range inputs {
					if in == fnames[1] {
						continue
					}
					newinputs = append(newinputs, in)
				}
				inputs = newinputs
			}
			continue

		case "unlink", "unlinkat":
			var newoutputs []string
			for _, out := range outputs {
				if out == fnames[0] {
					continue
				}
				newoutputs = append(newoutputs, out)
			}
			outputs = newoutputs
			continue
		}
		if !oseen[fnames[0]] {
			outputs = append(outputs, fnames[0])
			oseen[fnames[0]] = true
			if iseen[fnames[0]] {
				var newinputs []string
				for _, in := range inputs {
					if in == fnames[0] {
						continue
					}
					newinputs = append(newinputs, in)
				}
				inputs = newinputs
			}
		}
	}
	return inputs, outputs
}

func nextLine(buf []byte) (line, remain []byte) {
	i := bytes.IndexByte(buf, '\n')
	if i < 0 {
		return buf, nil
	}
	return buf[:i], buf[i+1:]
}

func parseTraceLine(ctx context.Context, line []byte) (sycall string, fnames []string, wr bool) {
	// line:
	// <pid> access(<path>, ...
	// <pid> chdir(<path>
	// <pid> creat(<path>, ...
	// <pid> execve(<path>, [<args>...
	// <pid> getcwd(<path>, ...
	// <pid> lstat(<path>, ...
	// <pid> openat(AT_FDCWD, <path>, O_<flag>, ...
	// <pid> readline(<path>, ...
	// <pid> rename(<path>, <path>
	// <pid> stat(<path>, ...
	// <pid> symlink(<target>,<path>
	//
	// <pid> newfstatat(AT_FDCWD, <path>, ...
	// <pid> linkat(AT_FDCWD, <path>
	// <pid> renameat(AT_FDCWD, <path>, AT_CDCWD, <path>
	// <pid> statx(AT_FDCWD, <path>, ...
	// <pid> unlinkat(AT_FDCWD, <path>,
	// <pid> utimensat(
	//
	// TODO(b/272383202): chdir changes AT_FDCWD
	//
	// return value of syscall
	//  success
	//   syscall(....) = 0
	//   openat(...) = 3
	//  fail
	//   syscall(...) = -1 ENOENT (No such file or directory)
	if log.V(9) {
		clog.Infof(ctx, "trace line: %q", line)
	}

	// workaround for missing --successful-only
	i := bytes.LastIndexByte(line, '=')
	if i < 0 {
		// no return value?
		return "", nil, false
	}
	ret := bytes.TrimSpace(line[i+1:])
	if bytes.HasPrefix(ret, []byte{'-'}) {
		// ignore error calls. i.e. negative return value
		if log.V(9) {
			clog.Infof(ctx, "trace line[error]: %q", line)
		}
		return "", nil, false
	}

	// success case.
	i = bytes.IndexByte(line, ' ')
	if i < 0 {
		return "", nil, false
	}
	buf := line[i+1:]
	i = bytes.IndexByte(buf, '(')
	if i < 0 {
		return "", nil, false
	}
	syscall := string(bytes.TrimSpace(buf[:i]))
	buf = buf[i+1:]
	switch syscall {
	case "access", "chdir", "execve", "lstat", "readlink", "stat", "statfs", "listxattr":
		fname, _ := extractPath(buf, false)
		return syscall, []string{fname}, false

	case "faccessat", "faccessat2", "newfstatat", "readlinkat", "statx":
		fname, _ := extractPath(buf, true)
		return syscall, []string{fname}, false

	case "creat", "unlink", "chmod", "chown":
		fname, _ := extractPath(buf, false)
		return syscall, []string{fname}, true

	case "mkdir", "rmdir":
		fname, _ := extractPath(buf, false)
		return syscall, []string{fname}, true

	case "symlink", "link":
		_, buf := extractPath(buf, false)
		buf = bytes.TrimPrefix(buf, []byte(", "))
		targetName, _ := extractPath(buf, false)
		return syscall, []string{targetName}, true

	case "open":
		fname, buf := extractPath(buf, false)
		if bytes.Contains(buf, []byte("O_RDONLY")) {
			return syscall, []string{fname}, false
		}
		return syscall, []string{fname}, true
	case "openat":
		// openat(..., <path>
		// skip AT_FDCWD etc.
		fname, buf := extractPath(buf, true)
		if bytes.Contains(buf, []byte("O_RDONLY")) {
			return syscall, []string{fname}, false
		}
		return syscall, []string{fname}, true

	case "unlinkat", "mkdirat":
		fname, _ := extractPath(buf, true)
		return syscall, []string{fname}, true

	case "rename":
		oldname, buf := extractPath(buf, false)
		buf = bytes.TrimPrefix(buf, []byte(", "))
		newname, _ := extractPath(buf, false)
		return syscall, []string{oldname, newname}, true

	case "linkat", "renameat":
		oldname, buf := extractPath(buf, true)
		buf = bytes.TrimPrefix(buf, []byte(", "))
		newname, _ := extractPath(buf, true)
		return syscall, []string{oldname, newname}, true

	case "utimensat", "getcwd":
		// ignore?
		return syscall, nil, false
	case "????":
		// <unfinished ...>
		return syscall, nil, false
	default:
		clog.Warningf(ctx, "unknown syscall=%q(%q", syscall, buf)
		return syscall, nil, false
	}
}

func extractPath(buf []byte, skipAt bool) (string, []byte) {
	if skipAt {
		i := bytes.IndexByte(buf, ',')
		if i < 0 {
			return "", buf
		}
		buf = buf[i+1:]
	}
	buf = bytes.TrimSpace(buf)
	if len(buf) == 0 {
		return "", nil
	}
	if buf[0] != '"' {
		return "", nil
	}
	buf = buf[1:]
	// need to support escaped " ?
	i := bytes.IndexByte(buf, '"')
	if i < 0 {
		return "", nil
	}
	return string(buf[:i]), buf[i+1:]
}
