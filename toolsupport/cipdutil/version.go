// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cipdutil

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// VersionInfo describes JSON file with cipd package version information.
type VersionInfo struct {
	PackageName string `json:"package_name"`
	InstanceID  string `json:"instance_id"`
}

var (
	startupVersionFile VersionInfo
	startupVersionErr  error
)

// StartupVersion returns value of version file as it was when the process
// has just started.
func StartupVersion() (VersionInfo, error) {
	return startupVersionFile, startupVersionErr
}

func init() {
	path, err := os.Executable()
	if err != nil {
		startupVersionErr = err
		return
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		startupVersionErr = err
		return
	}
	path, err = filepath.Abs(path)
	if err != nil {
		startupVersionErr = err
		return
	}
	verfile := filepath.Join(filepath.Dir(path), ".versions", filepath.Base(path)+".cipd_version")
	f, err := os.Open(verfile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			startupVersionErr = err
		}
		return
	}
	defer f.Close()
	startupVersionErr = json.NewDecoder(f).Decode(&startupVersionFile)
}
