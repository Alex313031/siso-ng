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
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/o11y/clog"
)

type localSource struct {
	wd       string
	stateDir string
}

func newLocalSource(ctx context.Context, outDir ninjabuild.DirFlag, stateDir string) (*localSource, error) {
	_, workspaceRoot, dir, err := ninjabuild.InitDir(ctx, outDir)
	if err != nil {
		return nil, fmt.Errorf("failed to init dir %s: %w", outDir, err)
	}
	wd := filepath.Join(workspaceRoot, dir)
	return &localSource{wd: wd, stateDir: stateDir}, nil
}

func (s *localSource) location() string {
	return s.wd
}

func (s *localSource) text() string { return "" }

func (s *localSource) fetch(ctx context.Context) (build.ProgressInfo, error) {
	portFilename := filepath.Join(s.stateDir, ".siso_port")
	buf, err := os.ReadFile(portFilename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return build.ProgressInfo{}, fmt.Errorf("siso is not running in %s?", s.wd)
		}
		return build.ProgressInfo{}, fmt.Errorf("siso is not running in %s? failed to read %q: %w", s.wd, portFilename, err)
	}
	addr := strings.TrimSpace(string(buf))
	progress, statusCode, err := fetchJSON[build.ProgressInfo](ctx, fmt.Sprintf("http://%s/api/progress", addr), portFilename)
	if statusCode == http.StatusNotFound {
		activeSteps, _, err := fetchJSON[[]build.ActiveStepInfo](ctx, fmt.Sprintf("http://%s/api/active_steps", addr), portFilename)
		if err != nil {
			return build.ProgressInfo{}, err
		}
		return build.ProgressInfo{
			ActiveSteps: activeSteps,
		}, nil
	}
	if err != nil {
		return build.ProgressInfo{}, err
	}
	return progress, nil
}

func fetchJSON[T any](ctx context.Context, url, portFilename string) (T, int, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return zero, 0, fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, 0, fmt.Errorf("failed to get %s via %q: %w", url, portFilename, err)
	}
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			clog.Warningf(ctx, "close %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return zero, resp.StatusCode, fmt.Errorf("%s error: %d %s", url, resp.StatusCode, resp.Status)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, resp.StatusCode, fmt.Errorf("%s read error: %w", url, err)
	}
	var val T
	err = json.Unmarshal(buf, &val)
	if err != nil {
		return zero, resp.StatusCode, fmt.Errorf("%s unmarshal error: %w", url, err)
	}
	return val, resp.StatusCode, nil
}
