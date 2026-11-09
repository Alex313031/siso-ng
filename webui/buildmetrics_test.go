// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/build/metadata"
	"go.chromium.org/build/siso/webui/invocation"
)

var _ invocation.Invocation = (*buildMetrics)(nil)

func mustWriteMetrics(t *testing.T, dir, content string) string {
	t.Helper()
	metricsPath := filepath.Join(dir, "siso_metrics.json")
	if err := os.WriteFile(metricsPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", metricsPath, err)
	}
	return metricsPath
}

func mustWriteMetadata(t *testing.T, dir string, info *metadata.InvocationInfo) {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("could not marshal %T: %v", info, err)
	}
	metadataPath := filepath.Join(dir, "siso_metadata.json")
	if err := os.WriteFile(metadataPath, data, 0644); err != nil {
		t.Fatalf("failed to write %s: %v", metadataPath, err)
	}
}

func TestBuildMetrics_CriticalPath(t *testing.T) {
	metricsPath := mustWriteMetrics(t, t.TempDir(), `{"build_id": "test-build-123"}
{"step_id": "step-1", "rule": "cc", "action": "clang", "outputs": ["foo.o"]}
{"step_id": "step-2", "rule": "cc", "action": "clang", "outputs": ["bar.o"], "prev": "step-1"}
{"step_id": "step-3", "rule": "cc", "action": "clang", "outputs": ["baz.o"], "prev": "step-1"}
{"step_id": "step-4", "rule": "link", "action": "ld", "outputs": ["app"], "prev": "step-3"}
`)

	bm, err := loadBuildMetrics(metricsPath)
	if err != nil {
		t.Fatalf("loadBuildMetrics() error = %v", err)
	}

	var inv invocation.Invocation = bm
	critPath := inv.CriticalPath()

	var gotIDs []string
	for _, step := range critPath {
		gotIDs = append(gotIDs, step.StepID)
	}

	wantIDs := []string{"step-1", "step-3", "step-4"}
	if diff := cmp.Diff(wantIDs, gotIDs); diff != "" {
		t.Errorf("CriticalPath() stepIDs mismatch (-want +got):\n%s", diff)
	}
}

func TestBuildMetrics_CriticalPath_SingleStep(t *testing.T) {
	metricsPath := mustWriteMetrics(t, t.TempDir(), `{"build_id": "test-build-single"}
{"step_id": "step-1", "rule": "cc", "action": "clang", "outputs": ["foo.o"]}
`)

	bm, err := loadBuildMetrics(metricsPath)
	if err != nil {
		t.Fatalf("loadBuildMetrics() error = %v", err)
	}

	critPath := bm.CriticalPath()
	var gotIDs []string
	for _, step := range critPath {
		gotIDs = append(gotIDs, step.StepID)
	}

	wantIDs := []string{"step-1"}
	if diff := cmp.Diff(wantIDs, gotIDs); diff != "" {
		t.Errorf("CriticalPath() stepIDs mismatch (-want +got):\n%s", diff)
	}
}

func TestBuildMetrics_Started(t *testing.T) {
	startTime := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	fileMtime := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name        string
		metricsJSON string
		info        *metadata.InvocationInfo
		want        invocation.Timestamp
	}{
		{
			name:        "with_invocationinfo",
			metricsJSON: `{"build_id": "test-build"}`,
			info: &metadata.InvocationInfo{
				BuildID:   "test-build",
				StartTime: startTime,
			},
			want: invocation.Timestamp{Time: startTime, Inferred: false},
		},
		{
			name:        "without_invocationinfo",
			metricsJSON: `{"build_id": "test-build", "duration_nanos": 5000000000}`,
			// No invocation info, so fall back to inferred start time.
			want: invocation.Timestamp{Time: fileMtime.Add(-5 * time.Second), Inferred: true},
		},
		{
			name:        "buildid_mismatch",
			metricsJSON: `{"build_id": "old-build", "duration_nanos": 1000000000}`,
			info: &metadata.InvocationInfo{
				BuildID:   "new-build",
				StartTime: startTime,
			},
			// Build ID is wrong, so treat as if there was no invocation info.
			want: invocation.Timestamp{Time: fileMtime.Add(-1 * time.Second), Inferred: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			metricsPath := mustWriteMetrics(t, tmpDir, tc.metricsJSON)
			if tc.info != nil {
				mustWriteMetadata(t, tmpDir, tc.info)
			}
			if err := os.Chtimes(metricsPath, fileMtime, fileMtime); err != nil {
				t.Fatalf("os.Chtimes(%q, %v, %v) = %v; want nil err", metricsPath, fileMtime, fileMtime, err)
			}

			bm, err := loadBuildMetrics(metricsPath)
			if err != nil {
				t.Fatalf("loadBuildMetrics(%q) = _, %v; want nil err", metricsPath, err)
			}

			got := bm.Started()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("bm.Started() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildMetrics_Started_Standalone(t *testing.T) {
	startTime := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name        string
		metricsJSON string
		info        *metadata.InvocationInfo
		want        invocation.Timestamp
	}{
		{
			name:        "with_invocationinfo",
			metricsJSON: `{"build_id": "test-build"}`,
			info: &metadata.InvocationInfo{
				BuildID:   "test-build",
				StartTime: startTime,
			},
			want: invocation.Timestamp{Time: startTime, Inferred: false},
		},
		{
			name:        "without_invocationinfo",
			metricsJSON: `{"build_id": "test-build"}`,
			want:        invocation.Timestamp{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			metricsPath := mustWriteMetrics(t, tmpDir, tc.metricsJSON)
			if tc.info != nil {
				mustWriteMetadata(t, tmpDir, tc.info)
			}

			provider := makeMetricsFileProvider()
			series, err := provider.Get(metricsPath)
			if err != nil {
				t.Fatalf("provider.Get(%q) = _, %v; want nil err", metricsPath, err)
			}
			bm := series.Latest()

			got := bm.Started()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("bm.Started() diff (-want +got):\n%s", diff)
			}
		})
	}
}
