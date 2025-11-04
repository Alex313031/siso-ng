// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"testing"
)

func TestParseProcPressureMemorySome(t *testing.T) {
	buf := []byte(`some avg10=0.00 avg60=0.00 avg300=0.00 total=10270662
full avg10=0.00 avg60=0.00 avg300=0.00 total=9928704
`)
	got, err := parseProcPressureMemorySome(buf)
	want := int64(10270662)
	if got != want || err != nil {
		t.Errorf("parseProcPressureMemorySome(%q)=%d, %v; want=%d, nil", buf, got, err, want)
	}
}

func FuzzParseProcPressureMemorySome(f *testing.F) {
	f.Add([]byte(`some avg10=0.00 avg60=0.00 avg300=0.00 total=10270662
full avg10=0.00 avg60=0.00 avg300=0.00 total=9928704
`))
	f.Fuzz(func(t *testing.T, buf []byte) {
		parseProcPressureMemorySome(buf) //nolint:errcheck
	})
}
