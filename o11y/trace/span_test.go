// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package trace

import (
	"testing"
)

func TestSpan_ID_Nil(t *testing.T) {
	var s *Span
	traceID, spanID := s.ID("test-project")
	if traceID != "" || spanID != "" {
		t.Errorf("s.ID() = (%q, %q); want (\"\", \"\")", traceID, spanID)
	}
}

func TestTracer_Enabled(t *testing.T) {
	ctx := t.Context()

	// 1. Nil tracer should be disabled safely
	var nilTracer *Tracer
	if nilTracer.Enabled() {
		t.Error("nilTracer.Enabled() = true; want false")
	}

	// 2. Tracer with empty filename should be disabled
	emptyTracer, err := NewTracer(ctx, "")
	if err != nil {
		t.Fatalf("NewTracer(\"\") failed: %v", err)
	}
	if emptyTracer.Enabled() {
		t.Error("emptyTracer.Enabled() = true; want false")
	}

	// 3. Tracer with valid filename should be enabled
	tempDir := t.TempDir()
	tempFile := tempDir + "/trace.json"
	fileTracer, err := NewTracer(ctx, tempFile)
	if err != nil {
		t.Fatalf("NewTracer(%q) failed: %v", tempFile, err)
	}
	defer fileTracer.Close(ctx)
	if !fileTracer.Enabled() {
		t.Error("fileTracer.Enabled() = false; want true")
	}
}
