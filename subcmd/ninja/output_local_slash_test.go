// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"testing"
)

// TestInitOutputLocal_DirTargetSlashInvariance checks the output_local predicate decides the same for a directory output with or without a trailing slash.
// The flush path probes the slash form (mtimecheck.go) and the post-execution path the slash-stripped form (builder.go); a slash-sensitive predicate would flip a directory's local presence between a fresh build and a no-op rebuild.
func TestInitOutputLocal_DirTargetSlashInvariance(t *testing.T) {
	ctx := t.Context()
	// Basenames whose extension each slash-sensitive strategy keys on.
	cases := map[string][]string{
		"greedy":  {"obj/foo.stamp", "obj/bar.o", "obj/baz.pcm"},
		"minimum": {"gen/foo.json", "gen/bar.py", "gen/baz.h"},
	}
	for strategy, dirs := range cases {
		outputLocal, err := initOutputLocal(ctx, strategy)
		if err != nil {
			t.Fatalf("initOutputLocal(%q): %v", strategy, err)
		}
		for _, dir := range dirs {
			stripped := outputLocal(ctx, dir)
			slashed := outputLocal(ctx, dir+"/")
			if stripped != slashed {
				t.Errorf("%s: outputLocal(%q)=%t but outputLocal(%q)=%t; decision must not depend on the trailing slash",
					strategy, dir, stripped, dir+"/", slashed)
			}
		}
	}
}
