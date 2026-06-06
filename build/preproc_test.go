// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestUniqueFiles(t *testing.T) {
	testCases := []struct {
		name  string
		input [][]string
		want  []string
	}{
		{
			name:  "empty",
			input: [][]string{},
			want:  []string{},
		},
		{
			name:  "one slice no duplicates",
			input: [][]string{{"a", "b", "c"}},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "one slice with duplicates",
			input: [][]string{{"a", "b", "a"}},
			want:  []string{"a", "b"},
		},
		{
			name:  "one slice with empty string",
			input: [][]string{{"a", "", "b"}},
			want:  []string{"a", "b"},
		},
		{
			name:  "multiple slices no overlap",
			input: [][]string{{"a", "b"}, {"c", "d"}},
			want:  []string{"a", "b", "c", "d"},
		},
		{
			name:  "multiple slices with overlap",
			input: [][]string{{"a", "b"}, {"b", "c"}},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "multiple slices with empty strings and duplicates",
			input: [][]string{{"a", ""}, {"b", "a", ""}, {"c"}},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "all empty",
			input: [][]string{{""}, {""}},
			want:  []string{},
		},
		{
			name:  "nil input",
			input: nil,
			want:  []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// TODO(b/477751081): The order of results is important for deps-log correctness.
			// Investigate why changing the order breaks the deps log and fix it.
			// After that, uniqueFiles can be optimized for memory usage.
			got := uniqueFiles(tc.input...)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("uniqueFiles() returned diff (-want +got):\n%s", diff)
			}
		})
	}
}
