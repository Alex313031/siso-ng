// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"go.chromium.org/build/siso/build"
)

func (s *WebuiServer) handleOutdirWatch(w http.ResponseWriter, r *http.Request) {
	tmpl, err := s.loadView("build_watch.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r)
		return
	}

	series, err := s.invocationSeriesFor(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("can't find outdir for %s: %v", r.URL, err), w, r)
		return
	}
	outdirInfo, ok := series.(*outdirInfo)
	if !ok {
		s.renderBuildViewError(http.StatusNotFound, "this is not an outdir", w, r)
		return
	}

	portFile := filepath.Join(outdirInfo.path, ".siso_port")
	portData, err := os.ReadFile(portFile)
	var activeSteps []build.ActiveStepInfo
	var watchErr string

	if err != nil {
		watchErr = "No active build detected (could not read .siso_port)"
	} else {
		addr := string(bytes.TrimSpace(portData))
		resp, err := http.Get(fmt.Sprintf("http://%s/api/active_steps", addr))
		if err != nil {
			watchErr = fmt.Sprintf("Failed to connect to statusz server: %v", err)
		} else {
			defer resp.Body.Close()
			err = json.NewDecoder(resp.Body).Decode(&activeSteps)
			if err != nil {
				watchErr = fmt.Sprintf("Failed to decode active steps: %v", err)
			}
		}
	}

	data := map[string]any{
		"activeSteps": activeSteps,
		"watchErr":    watchErr,
	}

	err = s.renderBuildView(w, r, tmpl, data)
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r)
	}
}
