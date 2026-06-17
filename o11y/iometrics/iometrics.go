// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package iometrics manages I/O metrics.
package iometrics

import "sync"

// IOMetrics holds I/O metrics.
type IOMetrics struct {
	name string

	mu sync.Mutex

	ops     int64
	opsErrs int64
	rOps    int64
	rBytes  int64
	rErrs   int64
	wOps    int64
	wBytes  int64
	wErrs   int64
}

// New returns new iometrics for name.
func New(name string) *IOMetrics {
	return &IOMetrics{name: name}
}

// OpsDone counts when a non read/write I/O operation is done. err is an I/O operation error.
// e.g. stats, mkdir, readlink etc
func (m *IOMetrics) OpsDone(err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops++
	if err != nil {
		m.opsErrs++
	}
}

// ReadDone counts when a read operation is done.
// n is the number of bytes, and err is a read error.
func (m *IOMetrics) ReadDone(n int, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rOps++
	m.rBytes += int64(n)
	if err != nil {
		m.rErrs++
	}
}

// WriteDone counts when a write operation is done.
// n is the number of bytes, and err is a write error.
func (m *IOMetrics) WriteDone(n int, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wOps++
	m.wBytes += int64(n)
	if err != nil {
		m.wErrs++
	}
}

// Name returns the name of the iometrics.
func (m *IOMetrics) Name() string {
	if m == nil {
		return "<nil>"
	}
	return m.name
}

// Stats holds iometrics.
type Stats struct {
	// Number of I/O operations other than reads and writes.
	Ops int64
	// Number of I/O operation errors other than read and write errors.
	OpsErrs int64

	// Number of read operations.
	ROps int64
	// Number of read bytes.
	RBytes int64
	// Number of read errors.
	RErrs int64

	// Number of write operations.
	WOps int64
	// Number of write bytes.
	WBytes int64
	// Number of write errors.
	WErrs int64
}

// Stats returns the snapshopt of the iometrics.
func (m *IOMetrics) Stats() Stats {
	if m == nil {
		return Stats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{
		Ops:     m.ops,
		OpsErrs: m.opsErrs,
		ROps:    m.rOps,
		RBytes:  m.rBytes,
		RErrs:   m.rErrs,
		WOps:    m.wOps,
		WBytes:  m.wBytes,
		WErrs:   m.wErrs,
	}
}
