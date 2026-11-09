// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ps

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStdoutURLSource_Progress(t *testing.T) {
	ctx := t.Context()

	logContent := `
random log line
[10/50] 0.5s S step1
[11/50] 1.0s S step2
[12/50] 1.5s F step1
`
	src := &stdoutURLSource{
		stdoutURL: "http://example.com/log",
		started:   time.Now(),
		done:      make(chan bool),
	}
	body := io.NopCloser(strings.NewReader(logContent))
	go src.run(ctx, body)

	select {
	case <-src.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for run to complete")
	}

	progress, err := src.fetch(ctx)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("fetch failed: %v", err)
	}

	if progress.Done != 12 {
		t.Errorf("progress.Done = %d, want 12", progress.Done)
	}
	if progress.Total != 50 {
		t.Errorf("progress.Total = %d, want 50", progress.Total)
	}
	if len(progress.ActiveSteps) != 1 {
		t.Fatalf("len(progress.ActiveSteps) = %d, want 1", len(progress.ActiveSteps))
	}
	if progress.ActiveSteps[0].Desc != "step2" {
		t.Errorf("ActiveSteps[0].Desc = %q, want %q", progress.ActiveSteps[0].Desc, "step2")
	}
}
