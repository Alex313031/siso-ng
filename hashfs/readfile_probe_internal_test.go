// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"io"
	"testing"
)

// probeReader returns a fixed (n, err) from Read, to exercise io.Reader edge
// cases like a byte delivered together with io.EOF.
type probeReader struct {
	n   int
	err error
}

func (r probeReader) Read(p []byte) (int, error) {
	for i := 0; i < r.n && i < len(p); i++ {
		p[i] = 'x'
	}
	return r.n, r.err
}

// TestProbeAtEOF: buf is the whole file only when the probe reads 0 bytes at
// io.EOF; a byte with io.EOF (allowed by io.Reader) means the file grew.
func TestProbeAtEOF(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		err  error
		want bool
	}{
		{"eof-zero-byte", 0, io.EOF, true},
		{"eof-with-byte", 1, io.EOF, false},
		{"byte-no-eof", 1, nil, false},
		{"zero-no-eof", 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeAtEOF(probeReader{n: tc.n, err: tc.err}); got != tc.want {
				t.Errorf("probeAtEOF(n=%d, err=%v) = %t; want %t", tc.n, tc.err, got, tc.want)
			}
		})
	}
}
