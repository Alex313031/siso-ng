// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui_test

import (
	"testing"

	"go.chromium.org/build/siso/ui"
)

func TestStripANSIEscapeCodes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{
			in:   "foo\033",
			want: "foo",
		},
		{
			in:   "foo\033[",
			want: "foo",
		},
		{
			in:   "\033[1maffixmgr.cxx:286:15: \033[0m\033[0;1;35mwarning: \033[0m\033[1musing the result... [-Wparentheses]\033[0m",
			want: "affixmgr.cxx:286:15: warning: using the result... [-Wparentheses]",
		},
	} {
		got := ui.StripANSIEscapeCodes(tc.in)
		if got != tc.want {
			t.Errorf("ui.StripANSIEscapeCodes(%q)=%q; want=%q", tc.in, got, tc.want)
		}
	}
}
