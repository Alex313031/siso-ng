// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui_test

import (
	"testing"
	"time"

	"go.chromium.org/build/siso/ui"
)

func TestFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		dur  time.Duration
		want string
	}{
		{
			want: "0.00s",
		},
		{
			dur:  1 * time.Millisecond,
			want: "0.00s",
		},
		{
			dur:  10 * time.Millisecond,
			want: "0.01s",
		},
		{
			dur:  1 * time.Second,
			want: "1.00s",
		},
		{
			dur:  1 * time.Minute,
			want: "1m00.00s",
		},
		{
			dur:  1*time.Minute + 1*time.Second + 100*time.Millisecond,
			want: "1m01.10s",
		},
		{
			dur:  1*time.Hour + 1*time.Minute + 1*time.Second + 100*time.Millisecond,
			want: "1h1m01.10s",
		},
		{
			dur:  1*time.Hour + 12*time.Minute + 34*time.Second + 100*time.Millisecond,
			want: "1h12m34.10s",
		},
	} {
		got := ui.FormatDuration(tc.dur)
		if got != tc.want {
			t.Errorf("ui.FormatDuration(%v)=%q; want=%q", tc.dur, got, tc.want)
		}
	}
}
