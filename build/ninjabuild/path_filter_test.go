// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import "testing"

func TestPathFilter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pf         *PathFilter
		matches    []string
		nonmatches []string
	}{
		{
			name: "starAllIncludes",
			pf: &PathFilter{
				Includes: []string{"*"},
			},
			matches: []string{
				"foo",
				"foo/bar",
			},
		},
		{
			name: "suffixIncludes",
			pf: &PathFilter{
				Includes: []string{"*.json", "*.ts"},
			},
			matches: []string{
				"foo.json",
				"foo/bar.json",
				"foo.ts",
				"foo/bar.ts",
			},
			nonmatches: []string{
				"foo.js",
				"foo/bar.js",
				"foo.json/bar",
			},
		},
		{
			name: "baseIncludes",
			pf: &PathFilter{
				Includes: []string{"foo-?.txt"},
			},
			matches: []string{
				"foo-1.txt",
				"bar/foo-1.txt",
			},
			nonmatches: []string{
				"foo-11.txt",
				"foo-1.txt/bar",
			},
		},
		{
			name: "pathIncludes",
			pf: &PathFilter{
				Includes: []string{"foo/bar-?.txt"},
			},
			matches: []string{
				"foo/bar-1.txt",
				"foo/bar-2.txt",
			},
			nonmatches: []string{
				"foo/bar-11.txt",
				"x/foo/bar-1.txt",
			},
		},
		{
			name: "starAllExcludes",
			pf: &PathFilter{
				Excludes: []string{"*"},
			},
			nonmatches: []string{
				"foo",
				"foo/bar",
			},
		},
		{
			name: "suffixExcludes",
			pf: &PathFilter{
				Excludes: []string{"*.json", "*.ts"},
			},
			matches: []string{
				"foo.js",
				"foo/bar.js",
				"foo.json/bar",
			},
			nonmatches: []string{
				"foo.json",
				"foo/bar.json",
				"foo.ts",
				"foo/bar.ts",
			},
		},
		{
			name: "baseExcludes",
			pf: &PathFilter{
				Excludes: []string{"foo-?.txt"},
			},
			matches: []string{
				"foo-11.txt",
				"foo-1.txt/bar",
			},
			nonmatches: []string{
				"foo-1.txt",
				"bar/foo-1.txt",
			},
		},
		{
			name: "pathExcludes",
			pf: &PathFilter{
				Excludes: []string{"foo/bar-?.txt"},
			},
			matches: []string{
				"foo/bar-11.txt",
				"x/foo/bar-1.txt",
			},
			nonmatches: []string{
				"foo/bar-1.txt",
				"foo/bar-2.txt",
			},
		},
		{
			name: "ExcludeAndIncludes",
			pf: &PathFilter{
				Excludes: []string{"foo*"},
				Includes: []string{"*.js"},
			},
			matches: []string{
				"bar.js",
				"foo/bar.js",
			},
			nonmatches: []string{
				"foo.js",
				"foo/bar.ts",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			f := tc.pf.filter(ctx, t.Name())
			for _, p := range tc.matches {
				if !f(ctx, p, false) {
					t.Errorf("f(ctx, %q, false)=false; want=true", p)
				}
			}
			for _, p := range tc.nonmatches {
				if f(ctx, p, false) {
					t.Errorf("f(ctx, %q, false)=true; want=false", p)
				}
			}
		})
	}
}
