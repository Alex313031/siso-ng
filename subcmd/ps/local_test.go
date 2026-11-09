// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ps

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build"
)

func TestLocalSource_Progress(t *testing.T) {
	ctx := t.Context()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/progress":
			p := build.ProgressInfo{
				Done:    20,
				Total:   100,
				Skipped: 5,
				ActiveSteps: []build.ActiveStepInfo{
					{Desc: "step1", Phase: "remote", Dur: "1.2s"},
				},
			}
			json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	portFile := filepath.Join(tempDir, ".siso_port")
	err := os.WriteFile(portFile, []byte(server.Listener.Addr().String()), 0644)
	if err != nil {
		t.Fatal(err)
	}

	src := &localSource{
		wd:       tempDir,
		stateDir: tempDir,
	}

	progress, err := src.fetch(ctx)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if progress.Done != 20 {
		t.Errorf("progress.Done = %d, want 20", progress.Done)
	}
	if progress.Total != 100 {
		t.Errorf("progress.Total = %d, want 100", progress.Total)
	}
	if len(progress.ActiveSteps) != 1 {
		t.Fatalf("len(progress.ActiveSteps) = %d, want 1", len(progress.ActiveSteps))
	}
}

func TestLocalSource_Fallback(t *testing.T) {
	ctx := t.Context()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/active_steps":
			steps := []build.ActiveStepInfo{
				{Desc: "step1", Phase: "remote", Dur: "1.2s"},
			}
			json.NewEncoder(w).Encode(steps)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	portFile := filepath.Join(tempDir, ".siso_port")
	err := os.WriteFile(portFile, []byte(server.Listener.Addr().String()), 0644)
	if err != nil {
		t.Fatal(err)
	}

	src := &localSource{
		wd:       tempDir,
		stateDir: tempDir,
	}

	progress, err := src.fetch(ctx)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if progress.Done != 0 || progress.Total != 0 {
		t.Errorf("progress = %+v, want Done=0, Total=0", progress)
	}
	if len(progress.ActiveSteps) != 1 {
		t.Fatalf("len(progress.ActiveSteps) = %d, want 1", len(progress.ActiveSteps))
	}
}
