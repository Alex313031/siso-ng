// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import "testing"

func TestElideMiddle(t *testing.T) {
	for _, tc := range []struct {
		msg   string
		width int
		want  string
	}{
		{
			msg:   "ACTION //third_party/perfetto/protos/perfetto/metrics/webview:descriptor_gen(//build/toolchain/android:android_clang_arm)",
			width: 80,
			want:  "ACTION //third_party/perfetto/protos/p...d/toolchain/android:android_clang_arm)",
		},
		{
			msg:   "pre: 0 local:\033[41m653\033[0m remote:\033[41m12345\033[0m cache: 0.67% fallback: \033[41m873\033[0m",
			width: 80,
			want:  "pre: 0 local:\033[41m653\033[0m remote:\033[41m12345\033[0m cache: 0.67% fallback: \033[41m873\033[0m",
		},
		{
			msg:   "pre: 0 local:\033[41m653\033[0m remote:\033[41m12345\033[0m",
			width: 18,
			want:  "pre: 0 ...e:\033[41m12345\033[0m",
		},
	} {
		got := elideMiddle(tc.msg, tc.width)
		if got != tc.want {
			t.Errorf("elideMiddle(%q, %d)=%q; want %q\nmsg:\n%s\ngot:\n%s", tc.msg, tc.width, got, tc.want, tc.msg, got)
		}
	}
}
