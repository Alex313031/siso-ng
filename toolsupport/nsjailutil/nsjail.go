// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package nsjailutil provides utilities for nsjail.
package nsjailutil

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	pb "go.chromium.org/build/siso/toolsupport/nsjailutil/proto"
)

// NSJail manages nsjail environment.
type NSJail struct {
	exePath string
	dir     string
	config  *pb.NsJailConfig
}

// Request is a request for nsjail.
type Request struct {
	// nsjail binary path.
	ExePath string `json:"nsjail_path"`

	// nsjail temporary scratch dir.
	JailRootDir string `json:"root_dir"`

	// absolute paths. e.g. /bin
	PublicDirs []string `json:"public_dirs,omitempty"`

	WorkspaceRoot string `json:"workspace_root"`    // absolute path to workspace root
	WorkDir       string `json:"workdir,omitempty"` // working dir, relative to WorkspaceRoot

	// relative to workspace.
	Inputs []string `json:"inputs"`

	// relative to workspace, or absolute path. read/write.
	OutDir string `json:"out_dir"`

	// relative to workspace, or absolute path
	Outputs []string `json:"outputs"`
}

const (
	// always uses execRootInSandbox for exec root
	// to detect unexpected usage of the absolute path
	// in the action.
	// TODO: no need to support input_root_absolute_path case?
	execRootInSandbox = "/src"
)

// New creates nsjail environment with req from fsys at root dir.
func New(ctx context.Context, fsys fs.FS, req Request) (_ *NSJail, err error) {
	jail := &NSJail{
		config: &pb.NsJailConfig{},
	}
	defer func() {
		if err != nil && jail != nil && jail.dir != "" {
			os.RemoveAll(jail.dir)
		}
	}()
	jail.exePath, err = exec.LookPath(req.ExePath)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(req.JailRootDir) {
		return nil, fmt.Errorf("root_dir is not absolute path: %q", req.JailRootDir)
	}
	if !filepath.IsAbs(req.WorkspaceRoot) {
		return nil, fmt.Errorf("workspace root is not absolute path: %q", req.WorkspaceRoot)
	}
	if filepath.IsAbs(req.WorkDir) {
		return nil, fmt.Errorf("ninja dir is not relative path: %q", req.WorkDir)
	}
	fsys, err = fs.Sub(fsys, strings.TrimPrefix(req.WorkspaceRoot, "/"))
	if err != nil {
		return nil, err
	}
	jail.dir, err = os.MkdirTemp(req.JailRootDir, "nsjail.*")
	if err != nil {
		return nil, err
	}
	for _, output := range req.Outputs {
		if filepath.IsAbs(output) {
			return nil, fmt.Errorf("output is absolute path: %q", output)
		}
		outputPathInSandbox := filepath.Join(execRootInSandbox, output)
		err := os.MkdirAll(filepath.Dir(filepath.Join(jail.dir, outputPathInSandbox)), 0755)
		if err != nil {
			return nil, err
		}
	}
	// equivalent to -q, for quiet execution
	jail.config.LogLevel = pb.LogLevel_WARNING.Enum()
	// disable all rlimits, default to limits set by parent.
	jail.config.DisableRl = proto.Bool(true)
	// nsjail has a 10 minute time limit by default, disable it.
	jail.config.TimeLimit = proto.Uint32(0)

	jail.config.Cwd = proto.String(filepath.Join(execRootInSandbox, req.WorkDir))
	// TODO: better environment variable sandboxing
	jail.config.KeepEnv = proto.Bool(true)

	// Some build systems (android) set TMPDIR to a folder in their
	// output directory to avoid memory costs of using /tmp. The absolute
	// path to that folder won't work when in the jail due to the remounting
	// to /src. Since we mount a dedicated folder to /tmp anyways, rewrite
	// TMPDIR to point to it.
	// TODO: Consider adding customizable env vars in the ninja file.
	jail.config.Envar = append(jail.config.Envar, "TMPDIR=/tmp")

	if req.PublicDirs == nil {
		req.PublicDirs = []string{
			"/bin",
			"/lib",
			"/lib64",
			"/usr/bin",
			"/usr/lib",
			"/usr/lib32",
			"/usr/lib64",
			"/dev",
		}
	}
	if paths := os.Getenv("SISO_NSJAIL_PUBLIC_DIRS"); paths != "" {
		seen := make(map[string]bool)
		for _, path := range filepath.SplitList(paths) {
			if !filepath.IsAbs(path) {
				path = filepath.Join(req.WorkspaceRoot, req.WorkDir, path)
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			req.PublicDirs = append(req.PublicDirs, path)
		}
	}
	for _, dir := range req.PublicDirs {
		jail.config.Mount = append(jail.config.Mount, &pb.MountPt{
			Src:    proto.String(dir),
			Dst:    proto.String(dir),
			IsBind: proto.Bool(true),
			IsDir:  proto.Bool(true),
		})
	}
	jail.config.MountProc = proto.Bool(true)
	// Add a temp directory. Using a directory in the working directory
	// instead of a tmpfs mount so that we don't have to worry about how
	// big of a tmpfs to make.
	tmpDir := filepath.Join(jail.dir, "tmp")
	err = os.Mkdir(tmpDir, 0755)
	if err != nil {
		return nil, err
	}
	jail.config.Mount = append(jail.config.Mount, &pb.MountPt{
		Src:    proto.String(tmpDir),
		Dst:    proto.String("/tmp"),
		IsBind: proto.Bool(true),
		IsDir:  proto.Bool(true),
		Rw:     proto.Bool(true),
	})

	// Add the source directory. Normally this is not necessary,
	// because the -R flags for the input files would cause nsjail
	// to create it. But in cases where the action has no inputs
	// or all the inputs are from an absolute out/ directory,
	// it won't be created by nsjail automatically and the --cwd /src
	// flag will fail.
	srcDir := filepath.Join(jail.dir, execRootInSandbox)
	err = os.MkdirAll(srcDir, 0755)
	if err != nil {
		return nil, err
	}
	jail.config.Mount = append(jail.config.Mount, &pb.MountPt{
		Src:    proto.String(srcDir),
		Dst:    proto.String(execRootInSandbox),
		IsBind: proto.Bool(true),
		IsDir:  proto.Bool(true),
	})

	// Add the out directory.
	// We don't want to encode the absolute path into output files,
	// so use /src/$OUT_DIR as output dir. b/479926946
	if filepath.IsAbs(req.OutDir) {
		return nil, fmt.Errorf("output dir is absolute path: %q", req.OutDir)
	}
	outDirInSandbox := filepath.Join(execRootInSandbox, req.OutDir)
	absOutDir := filepath.Join(jail.dir, outDirInSandbox)
	err = os.MkdirAll(absOutDir, 0755)
	if err != nil {
		return nil, err
	}
	jail.config.Mount = append(jail.config.Mount, &pb.MountPt{
		Src:    proto.String(absOutDir),
		Dst:    proto.String(outDirInSandbox),
		IsBind: proto.Bool(true),
		IsDir:  proto.Bool(true),
		Rw:     proto.Bool(true),
	})

	for _, input := range req.Inputs {
		absInputPath := filepath.Join(req.WorkspaceRoot, input)
		inputPathInSandbox := filepath.Join(execRootInSandbox, input)
		fi, err := fs.Lstat(fsys, input)
		if err != nil {
			return nil, err
		}
		switch {
		case fi.Mode()&fs.ModeType == fs.ModeSymlink:
			// need to use symlink as symlink.
			// android uses dangling symlink, and
			// bind such dangling symlink will fail.
			target, err := fs.ReadLink(fsys, input)
			if err != nil {
				return nil, err
			}
			jail.config.Mount = append(jail.config.Mount, &pb.MountPt{
				Src:       proto.String(target),
				Dst:       proto.String(inputPathInSandbox),
				IsSymlink: proto.Bool(true),
				IsDir:     proto.Bool(false),
			})
		case fi.Mode().IsRegular():
			jail.config.Mount = append(jail.config.Mount, &pb.MountPt{
				Src:    proto.String(absInputPath),
				Dst:    proto.String(inputPathInSandbox),
				IsBind: proto.Bool(true),
				IsDir:  proto.Bool(false),
			})
		default:
			return nil, fmt.Errorf("unsupported input file type %q: %v", input, fi.Mode())
		}
	}
	return jail, nil
}

// Dir returns jail dir.
func (j *NSJail) Dir() string {
	return j.dir
}

// ExecRoot returns exec root dir in jail.
func (j *NSJail) ExecRoot() string {
	return filepath.Join(j.dir, execRootInSandbox)
}

// Args creates nsjail config to run args and returns command line to run args under nsjail.
func (j *NSJail) Args(ctx context.Context, args ...string) ([]string, error) {
	config := proto.CloneOf(j.config)
	arg0 := args[0]
	if !filepath.IsAbs(arg0) && !strings.ContainsRune(arg0, filepath.Separator) {
		exePath, err := exec.LookPath(arg0)
		if err != nil {
			return nil, fmt.Errorf("failed to lookpath %q: %v", arg0, err)
		}
		arg0 = exePath
	}

	config.ExecBin = &pb.Exe{
		Path: proto.String(arg0),
		Arg0: proto.String(args[0]),
		Arg:  args[1:],
	}

	configData, err := prototext.MarshalOptions{
		Multiline: true,
		Indent:    " ",
	}.Marshal(config)
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(j.dir, "nsjail.config")
	err = os.WriteFile(configPath, configData, 0644)
	if err != nil {
		return nil, err
	}
	return []string{j.exePath, "-C", configPath}, nil
}

// Close closes the jail and cleans up jail dir.
func (j *NSJail) Close() error {
	if j == nil || j.dir == "" {
		return nil
	}
	return os.RemoveAll(j.dir)
}
