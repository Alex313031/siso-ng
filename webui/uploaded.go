// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"fmt"
	"iter"
	"slices"
	"sync"

	"go.chromium.org/build/siso/webui/invocation"
)

type metricsFileProvider struct {
	mu    sync.Mutex
	files map[string]metricsFileInfo
}

func makeMetricsFileProvider() metricsFileProvider {
	return metricsFileProvider{
		files: make(map[string]metricsFileInfo),
	}
}

// Get loads metrics from the provided file.
func (u *metricsFileProvider) Get(metricsPath string) (invocation.Series[*buildMetrics], error) {
	metrics, err := loadBuildMetrics(metricsPath)
	if err != nil {
		return metricsFileInfo{}, fmt.Errorf("failed to import metrics from %q: %w", metricsPath, err)
	}
	metrics.standalone = true
	file := metricsFileInfo{
		path:    metricsPath,
		metrics: metrics,
	}
	u.mu.Lock()
	u.files[metricsPath] = file
	u.mu.Unlock()
	return file, nil
}

// Invalidate drops the file from the loaded metrics.
func (u *metricsFileProvider) Invalidate(metricsPath string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.files, metricsPath)
}

type metricsFileInfo struct {
	path    string
	metrics *buildMetrics
}

// All implements [invocation.Series].
// There is only one file in an uploaded series.
func (m metricsFileInfo) All() iter.Seq[*buildMetrics] {
	return slices.Values([]*buildMetrics{m.metrics})
}

// Get implements [invocation.Series].
func (m metricsFileInfo) Get(id string) *buildMetrics {
	if id != m.metrics.ID() {
		return nil
	}
	return m.metrics
}

// Latest implements [invocation.Series].
func (m metricsFileInfo) Latest() *buildMetrics {
	return m.metrics
}

// Title implements [invocation.Series].
func (m metricsFileInfo) Title() string {
	return "uploaded"
}
