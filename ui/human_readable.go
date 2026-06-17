// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import "fmt"

// NumBytes represents number of bytes.
type NumBytes int64

func (s NumBytes) String() string {
	var bytesUnit = map[int64]string{
		1 << 10: "KiB",
		1 << 20: "MiB",
		1 << 30: "GiB",
		1 << 40: "TiB",
	}
	for _, k := range []int64{1 << 40, 1 << 30, 1 << 20, 1 << 10} {
		if int64(s) >= k {
			return fmt.Sprintf("%.02f%s", float64(s)/float64(k), bytesUnit[k])
		}
	}
	return fmt.Sprintf("%dB", int64(s))
}
