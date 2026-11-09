// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package metadata_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"go.chromium.org/build/siso/build/metadata"
)

func TestInvocationInfo_CommandLineArgs(t *testing.T) {
	info := metadata.InvocationInfo{
		SisoVersion:        "v1.5.31",
		StartTime:          time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
		BuildID:            "build-123",
		Targets:            []string{"chrome", "base_unittests"},
		CommandLineArgs:    []string{"ninja", "-C", "out/Default", "chrome"},
		MetricsLabels:      map[string]string{"type": "cq"},
		EnabledExperiments: []string{"check-deps"},
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var got metadata.InvocationInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if !reflect.DeepEqual(got.CommandLineArgs, info.CommandLineArgs) {
		t.Errorf("CommandLineArgs = %v, want %v", got.CommandLineArgs, info.CommandLineArgs)
	}
}

func TestInvocationInfo_OmitEmptyCommandLineArgs(t *testing.T) {
	info := metadata.InvocationInfo{
		BuildID: "build-123",
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := raw["command_line_args"]; ok {
		t.Errorf("expected command_line_args to be omitted when empty, got %v", raw["command_line_args"])
	}
}
