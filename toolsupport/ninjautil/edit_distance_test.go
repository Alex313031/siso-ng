// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import "testing"

func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		s1, s2 string
		want   int
	}{
		{
			s1:   "",
			s2:   "ninja",
			want: 5,
		},
		{
			s1:   "ninja",
			s2:   "",
			want: 5,
		},
		{
			s1:   "",
			s2:   "",
			want: 0,
		},
		{
			s1:   "ninja",
			s2:   "njnja",
			want: 1,
		},
		{
			s1:   "njnja",
			s2:   "ninja",
			want: 1,
		},
		{
			s1:   "browser_tests",
			s2:   "browser_tests",
			want: 0,
		},
		{
			s1:   "browser_test",
			s2:   "browser_tests",
			want: 1,
		},
		{
			s1:   "browser_tests",
			s2:   "browser_test",
			want: 1,
		},
		{
			s1:   "chrome",
			s2:   "chorme",
			want: 2,
		},
		{
			s1:   "chorme",
			s2:   "chrome",
			want: 2,
		},
	} {
		got := editDistance(tc.s1, tc.s2, 0)
		if got != tc.want {
			t.Errorf("editDistance(%q, %q, 0)=%d; want=%d", tc.s1, tc.s2, got, tc.want)
		}
	}
}

func TestEditDistanceMax(t *testing.T) {
	const s1 = "abcdefghijklmnop"
	const s2 = "ponmlkjihgfedcba"
	for max := 1; max < 7; max++ {
		got := editDistance(s1, s2, max)
		if got != max+1 {
			t.Errorf("editDistance(%q, %q, %d)=%d; want=%d", s1, s2, max, got, max+1)
		}
	}
}
