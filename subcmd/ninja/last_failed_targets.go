// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"go.chromium.org/build/siso/o11y/clog"
)

const (
	// relative to -state_dir
	failedTargetsFile = ".siso_failed_targets"
)

// lastTargets contains targets and failed targets of the last build.
type lastTargets struct {
	Targets []string `json:"targets,omitempty"`
	Failed  []string `json:"failed,omitempty"`
}

func hasLastFailedTargets(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, failedTargetsFile))
	return err == nil
}

func removeLastFailedTargets(ctx context.Context, stateDir string) {
	targetsFile := filepath.Join(stateDir, failedTargetsFile)
	err := os.Remove(targetsFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		clog.Warningf(ctx, "failed to remove %s: %v", targetsFile, err)
	}

}

func saveLastFailedTargets(stateDir string, targets, failed []string) error {
	v := lastTargets{
		Targets: targets,
		Failed:  failed,
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal last targets: %w", err)
	}
	err = os.WriteFile(filepath.Join(stateDir, failedTargetsFile), buf, 0644)
	if err != nil {
		return fmt.Errorf("save last targets: %w", err)
	}
	return nil
}

// loadLastFailedTargets loads .siso_failed_targets and return the last failed targets
// if the last targets matches with the current targets.
func loadLastFailedTargets(ctx context.Context, stateDir string, targets []string) []string {
	targetsFile := filepath.Join(stateDir, failedTargetsFile)
	buf, err := os.ReadFile(targetsFile)
	if err != nil {
		clog.Warningf(ctx, "loadLastFailedTargets: %v", err)
		return nil
	}
	var last lastTargets
	err = json.Unmarshal(buf, &last)
	if err != nil {
		clog.Warningf(ctx, "loadLastFailedTargets: %v", err)
		return nil
	}
	if len(targets) != len(last.Targets) {
		return nil
	}
	sort.Strings(targets)
	sort.Strings(last.Targets)
	for i := range targets {
		if targets[i] != last.Targets[i] {
			return nil
		}
	}
	return last.Failed
}
