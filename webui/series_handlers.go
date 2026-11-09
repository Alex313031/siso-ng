// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"fmt"
	"net/http"
)

func (s *WebuiServer) handleInvocationSeriesList(w http.ResponseWriter, r *http.Request) {
	series, err := s.invocationSeriesFor(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("failed to load invocation(s) for %s: %v", r.URL, err), w, r)
		return
	}
	if series == nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no invocations found for %s", r.URL), w, r)
		return
	}

	tmpl, err := s.loadView("series_list.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r)
		return
	}

	err = s.renderBuildView(w, r, tmpl, map[string]any{
		"isSeriesList": true,
	})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r)
	}
}
