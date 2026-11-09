// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package invocation

import (
	"iter"
	"time"

	"go.chromium.org/build/siso/build"
)

// Provider is a strategy that owns one or more invocations.
//
// For example:
//
//   - An outdir provider owns the lifecycle of finding all outdirs (out/Default,
//     out/Release, etc.) under a single workspace.
//   - Uploaded metric providers own multiple separate siso_metrics.json.
//
// All invocations are returned grouped as a [Series].
type Provider[T Invocation] interface {
	Get(key string) (Series[T], error)
	Invalidate(key string)
}

// Series groups one or more invocations that are related to each other,
// and webui offers cross-comparison functionality with them.
//
// For example, all invocations in the same outdir should be one series,
//
// We don't know whether multiple siso_metrics.json uploads came from the same
// outdir, so those should be grouped by themselves.
type Series[T Invocation] interface {
	// Get returns an invocation from this series based on a known build ID.
	Get(id string) T
	// All returns an iterator over all invocations from this series.
	All() iter.Seq[T]
	// Latest returns the most recent invocation in this series.
	Latest() T
	// Title returns a human-readable title of this series (e.g. outdir path or "uploaded").
	Title() string
}

// Invocation represents data for a single build invocation.
type Invocation interface {
	// ID returns the build ID.
	ID() string
	// Started returns when the build started, or a zero value if unknown.
	Started() Timestamp
	// StepMetrics returns all build steps run in this invocation.
	StepMetrics() []*build.StepMetric
	// CriticalPath returns the build steps on the critical path in execution order.
	CriticalPath() []*build.StepMetric
}

// Timestamp represents either a known timestamp or one inferred from another source.
// For example if we only have a local step metrics file but build timestamp is unavailable,
// we could approximate the build time using the mtime of the step metrics.
type Timestamp struct {
	Time     time.Time
	Inferred bool
}

// IsZero reports whether t represents the zero time instant.
func (t Timestamp) IsZero() bool {
	return t.Time.IsZero()
}
