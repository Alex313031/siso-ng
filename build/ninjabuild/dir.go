// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.chromium.org/build/siso/o11y/clog"
)

// DirFlag contains ninja directory flags.
type DirFlag struct {
	Dir           string
	ConfigRepoDir string
}

// AndroidOutDir returns out directory in android build.
func AndroidOutDir() string {
	outDir := os.Getenv("OUT_DIR")
	if outDir != "" {
		return outDir
	}
	return "out"
}

func defaultConfigRepoDir() string {
	_, err := os.Stat("build/soong/siso_config")
	if err == nil {
		// android build has build/soong/siso_config
		// and prepares siso config in $OUT_DIR/siso_config.
		return filepath.Join(AndroidOutDir(), "siso_config")
	}
	// default is "build/config/siso.
	return "build/config/siso"
}

// RegisterFlags registers dir flags in fs.
func (f *DirFlag) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&f.Dir, "C", ".", "ninja running directory (chdir before run)")
	fs.StringVar(&f.ConfigRepoDir, "config_repo_dir", defaultConfigRepoDir(), "config repo directory (relative to workspace)")
}

// InitDir prepares the environment for a build by resolving the workspace.
//
// It resolves the current working directory (evaluating symlinks),
// changes the working directory to f.Dir, and
// searches upward for f.ConfigRepoDir.
//
// It returns:
// - startDir: The absolute, symlink-free original directory.
// - workspaceRoot: The absolute path to the workspace.
// - dir: The relative path from workspaceRoot to the new working directory.
//
// current working directory becomes workspaceRoot/dir, where
// workspaceRoot/f.ConfigRepoDir exists.
func InitDir(ctx context.Context, f DirFlag) (startDir, workspaceRoot, outDir string, _ error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "", "", err
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		return "", "", "", err
	}
	clog.Infof(ctx, "wd: %s", wd)
	startDir = wd
	workspaceRoot = startDir
	err = os.Chdir(f.Dir)
	if err != nil {
		return "", "", "", err
	}
	clog.Infof(ctx, "change dir to %s", f.Dir)
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", "", err
	}
	realCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		clog.Warningf(ctx, "failed to eval symlinks %q: %v", cwd, err)
	} else if cwd != realCWD {
		clog.Infof(ctx, "cwd %s -> %s", cwd, realCWD)
		cwd = realCWD
	}
	if !filepath.IsAbs(f.ConfigRepoDir) {
		workspaceRoot = detectWorkspaceRoot(cwd, f.ConfigRepoDir)
	}
	outDir, err = filepath.Rel(workspaceRoot, cwd)
	if err != nil {
		return "", "", "", err
	}
	if !filepath.IsLocal(outDir) {
		return "", "", "", fmt.Errorf("dir %q is outside of workspace %q", cwd, workspaceRoot)
	}
	return startDir, workspaceRoot, outDir, nil
}

// detectWorkspaceRoot detects workspace from path given marker.
// If the marker is not found in any parent directory, it falls back
// to using cwd as the workspace. This allows simple Ninja projects
// without Starlark configuration to work.
func detectWorkspaceRoot(cwd, marker string) string {
	dir := cwd
	for {
		_, err := os.Stat(filepath.Join(dir, marker))
		if err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Marker not found; use cwd as workspace.
			return cwd
		}
		dir = parent
	}
}
