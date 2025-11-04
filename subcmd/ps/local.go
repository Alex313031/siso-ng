// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/o11y/clog"
)

type localSource struct {
	wd       string
	stateDir string
}

func newLocalSource(dir, stateDir string) (*localSource, error) {
	err := os.Chdir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to chdir %s: %w", dir, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get wd: %w", err)
	}
	return &localSource{wd: wd, stateDir: stateDir}, nil
}

func (s *localSource) location() string {
	return s.wd
}

func (s *localSource) text() string { return "" }

func (s *localSource) fetch(ctx context.Context) ([]build.ActiveStepInfo, error) {
	portFilename := filepath.Join(s.stateDir, ".siso_port")
	buf, err := os.ReadFile(portFilename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("siso is not running in %s?", s.wd)
		}
		return nil, fmt.Errorf("siso is not running in %s? failed to read %s: %w", s.wd, portFilename, err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/api/active_steps", strings.TrimSpace(string(buf))), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get active_steps via %s: %w", portFilename, err)
	}
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			clog.Warningf(ctx, "close %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/api/active_steps error: %d %s", resp.StatusCode, resp.Status)
	}
	buf, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("/api/active_steps read error: %w", err)
	}
	var activeSteps []build.ActiveStepInfo
	err = json.Unmarshal(buf, &activeSteps)
	if err != nil {
		return nil, fmt.Errorf("/api/active_steps unmarshal error: %w", err)
	}
	return activeSteps, nil
}
