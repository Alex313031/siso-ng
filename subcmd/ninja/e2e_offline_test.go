// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"flag"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/ui"
)

func TestBuild_offline(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	uiDefault := ui.Default
	ui.Default = &ui.TermUI{}
	defer func() {
		ui.Default = uiDefault
	}()
	limits := build.DefaultLimits(ctx)
	defer func() {
		build.SetDefaultForTest(limits)
	}()
	limits.FastLocal = 0
	build.SetDefaultForTest(limits)

	testName := t.Name()

	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "basic",
		},
		{
			name: "phony",
			// intermediate dir of phony targets should not be created.
			args: []string{"output2", "output2/foo:foo"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupFiles(t, dir, testName, nil)
			t.Chdir(dir)
			ninja := &Command{}
			flagSet := flag.NewFlagSet("ninja", flag.ContinueOnError)
			ninja.SetFlags(flagSet)
			ninja.Flags = flagSet
			args := []string{"-C", "out/siso", "--offline"}
			args = append(args, tc.args...)
			err := flagSet.Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			stats, err := ninja.run(ctx)
			if err != nil {
				t.Errorf("ninja run failed: %v", err)
			}
			if stats.Done != stats.Total {
				t.Errorf("ninja stats.Done=%d; want=%d", stats.Done, stats.Total)
			}
			if stats.Local+stats.Skipped != stats.Total {
				t.Errorf("ninja stats.Local=%d + Skipped=%d; want=%d", stats.Local, stats.Skipped, stats.Total)
			}
			if stats.Fail != 0 {
				t.Errorf("ninja stats.Fail=%d; want=0", stats.Fail)
			}
		})
	}
}
