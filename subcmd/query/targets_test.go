// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"bytes"
	"flag"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// https://googleplex-android.googlesource.com/platform/external/n2/+/refs/heads/main/tests/e2e/tools/targets_test.rs
func TestTargets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		buildNinja string
		args       []string
		want       string
	}{
		{
			name: "deps",
			buildNinja: `
rule myrule
build a: myrule b c d
build b: myrule
build c: myrule
build d: myrule e
build e: myrule
build other_top_level: myrule
`,
			args: nil,
			want: `a: myrule
other_top_level: myrule
`,
		},
		{
			name: "deps-2",
			buildNinja: `
rule myrule
build a: myrule b c d
build b: myrule
build c: myrule
build d: myrule e
build e: myrule
build other_top_level: myrule
`,
			args: []string{"-depth", "2"},
			want: `a: myrule
 b: myrule
 c: myrule
 d: myrule
other_top_level: myrule
`,
		},
		{
			name: "deps-0",
			buildNinja: `
rule myrule
build a: myrule b c d
build b: myrule
build c: myrule
build d: myrule e
build e: myrule
build other_top_level: myrule
`,
			args: []string{"-depth", "0"},
			want: `a: myrule
 b: myrule
 c: myrule
 d: myrule
  e: myrule
other_top_level: myrule
`,
		},
		{
			name: "all",
			buildNinja: `
rule myrule
rule myrule2
build a: myrule b c d
build b: myrule
build c: myrule2
build d: myrule2 e
build e: myrule
build other_top_level: myrule
`,
			args: []string{"-all"},
			want: `a: myrule
b: myrule
c: myrule2
d: myrule2
e: myrule
other_top_level: myrule
`,
		},
		{
			name: "rule-myrule2",
			buildNinja: `rule myrule
rule myrule2
build a: myrule b c d in1
build b: myrule in2 in3
build c: myrule2
build d: myrule2 e
build e: myrule in3
build other_top_level: myrule
`,
			args: []string{"-rule", "myrule2"},
			want: `c
d
`,
		},
		{
			name: "rule-empty",
			buildNinja: `rule myrule
rule myrule2
build a: myrule b c d in1
build b: myrule in2 in3
build c: myrule2
build d: myrule2 e
build e: myrule in3
build other_top_level: myrule
`,
			args: []string{"-rule", ""},
			want: `in1
in2
in3
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			os.WriteFile("build.ninja", []byte(tc.buildNinja), 0644)
			var buf bytes.Buffer
			c := &targetsCommand{w: &buf}
			flagSet := flag.NewFlagSet("targets", flag.ContinueOnError)
			c.SetFlags(flagSet)
			err := flagSet.Parse(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			err = c.run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(buf.String(), tc.want); diff != "" {
				t.Errorf("query targets diff -want +got:\n%s", diff)
			}
		})
	}
}
