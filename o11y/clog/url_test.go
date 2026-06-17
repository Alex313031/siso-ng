// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package clog

import (
	"testing"
	"time"

	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

func TestURL(t *testing.T) {
	started := time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC)
	l := &Logger{
		res: &mrpb.MonitoredResource{
			Type: "generic_task",
			Labels: map[string]string{
				"project_id": "test-project",
				"task_id":    "test-task-id",
			},
		},
		started: started,
	}

	want := "https://console.cloud.google.com/logs/viewer?project=test-project&resource=generic_task/task_id/test-task-id&startTime=2026-06-15T14:55:00Z&endTime=2026-06-16T15:00:00Z"
	got := l.URL()
	if got != want {
		t.Errorf("URL() = %q; want %q", got, want)
	}
}

func TestURL_ZeroStarted(t *testing.T) {
	l := &Logger{
		res: &mrpb.MonitoredResource{
			Type: "generic_task",
			Labels: map[string]string{
				"project_id": "test-project",
				"task_id":    "test-task-id",
			},
		},
	}

	want := "https://console.cloud.google.com/logs/viewer?project=test-project&resource=generic_task/task_id/test-task-id"
	got := l.URL()
	if got != want {
		t.Errorf("URL() = %q; want %q", got, want)
	}
}

func TestURL_NilResource(t *testing.T) {
	l := &Logger{
		started: time.Now(),
	}
	got := l.URL()
	if got != "" {
		t.Errorf("URL() = %q; want %q", got, "")
	}
}
