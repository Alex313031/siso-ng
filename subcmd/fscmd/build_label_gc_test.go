// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestGcCommand_Run_ValidationErrors(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantErrSubstr string
	}{
		{
			name:          "no_args",
			args:          []string{},
			wantErrSubstr: "must specify either build labels to evict or --retain_last_x",
		},
		{
			name:          "both_args",
			args:          []string{"--retain_last_x=3", "label1"},
			wantErrSubstr: "cannot use both build labels and --retain_last_x simultaneously",
		},
		{
			name:          "negative_retain_last_x",
			args:          []string{"--retain_last_x=-1"},
			wantErrSubstr: "retain_last_x must be non-negative",
		},
		{
			name:          "list_active_labels_with_build_labels",
			args:          []string{"--list_active_labels", "some_label"},
			wantErrSubstr: "--list_active_labels cannot be used with",
		},
		{
			name:          "list_active_labels_with_retain_last_x",
			args:          []string{"--list_active_labels", "--retain_last_x=3"},
			wantErrSubstr: "--list_active_labels cannot be used with",
		},
		{
			name:          "list_active_labels_with_dry_run",
			args:          []string{"--list_active_labels", "--dry_run"},
			wantErrSubstr: "--list_active_labels cannot be used with",
		},
		{
			name:          "list_active_labels_with_output_file",
			args:          []string{"--list_active_labels", "--output_file=out.txt"},
			wantErrSubstr: "--list_active_labels cannot be used with",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &gcCommand{}
			fs := flag.NewFlagSet("gc", flag.ContinueOnError)
			cmd.SetFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("fs.Parse(%v) err = %v, want nil", tc.args, err)
			}
			cmd.Flags = fs

			err := cmd.run(t.Context())
			if !errors.Is(err, flag.ErrHelp) {
				t.Errorf("cmd.run(ctx) err = %v, want errors.Is(flag.ErrHelp)", err)
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("cmd.run(ctx) err = %v, want substring %q", err, tc.wantErrSubstr)
			}
		})
	}
}
