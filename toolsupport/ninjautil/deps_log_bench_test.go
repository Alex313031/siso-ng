// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkNewDepsLog measures loading a legacy deps log with 20,000
// outputs, each depending on 200 of 50,000 headers.
func BenchmarkNewDepsLog(b *testing.B) {
	ctx := b.Context()
	fname := filepath.Join(b.TempDir(), "ninja_deps")
	dl, err := NewDepsLog(ctx, fname)
	if err != nil {
		b.Fatal(err)
	}
	const (
		numOutputs = 20000
		numHeaders = 50000
		numDeps    = 200
	)
	headers := make([]string, numHeaders)
	for i := range headers {
		headers[i] = fmt.Sprintf("../../third_party/blink/renderer/core/dir%03d/header%05d.h", i%1000, i)
	}
	deps := make([]string, numDeps)
	mtime := time.Unix(1700000000, 0)
	for i := range numOutputs {
		for j := range deps {
			deps[j] = headers[(i*7+j*131)%numHeaders]
		}
		output := fmt.Sprintf("obj/third_party/blink/renderer/core/dir%03d/file%05d.o", i%1000, i)
		if _, err := dl.Record(ctx, output, mtime, deps); err != nil {
			b.Fatal(err)
		}
	}
	if err := dl.Close(); err != nil {
		b.Fatal(err)
	}

	for b.Loop() {
		dl, err := NewDepsLog(ctx, fname)
		if err != nil {
			b.Fatal(err)
		}
		if err := dl.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
