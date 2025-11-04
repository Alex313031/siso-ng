// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package ninjautil

import "testing"

func TestShellEscape(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{
			in:   "some/sensible/path/without/crazy/characters.c++",
			want: "some/sensible/path/without/crazy/characters.c++",
		},
		{
			in:   "foo bar\"/'$@d!st!c'/path'",
			want: "'foo bar\"/'\\''$@d!st!c'\\''/path'\\'''",
		},
	} {
		got := shellEscape(tc.in)
		if got != tc.want {
			t.Errorf("shellEscape(%q)=%q; want=%q", tc.in, got, tc.want)
		}
	}
}
