// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"flag"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseFlagsFully(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		want      []string
		wantDebug debugMode
	}{
		{
			name: "simple",
			args: []string{"-C", "out/siso"},
			want: nil,
		},
		{
			name: "target",
			args: []string{"-C", "out/siso", "-project", "rbe-chrome-untrusted", "chrome"},
			want: []string{"chrome"},
		},
		{
			name: "after-flag",
			args: []string{"-C", "out/siso", "-project", "rbe-chrome-untrusted", "chrome", "-d", "explain"},
			want: []string{"chrome"},
			wantDebug: debugMode{
				Explain: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Command{}
			flagSet := flag.NewFlagSet("ninja", flag.ContinueOnError)
			c.SetFlags(flagSet)
			err := flagSet.Parse(tc.args)
			if err != nil {
				t.Fatalf("flag parse %v; want nil err", err)
			}
			err = parseFlagsFully(flagSet)
			if err != nil {
				t.Fatalf("flag parse fully %v; want nil err", err)
			}
			if diff := cmp.Diff(tc.want, flagSet.Args()); diff != "" {
				t.Errorf("args diff -want +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantDebug, c.debugMode); diff != "" {
				t.Errorf("debugMode diff -want +got:\n%s", diff)
			}
		})
	}
}
