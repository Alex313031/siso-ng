// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"testing"
)

func TestNewOTELMetricsExporterInvalidAddress(t *testing.T) {
	got, err := newOTELMetricsExporter(t.Context(), "%")
	if err == nil {
		t.Fatal("newOTELMetricsExporter() succeeded, want invalid address error")
	}
	if got != nil {
		t.Errorf("newOTELMetricsExporter() = %T, want nil", got)
	}
}
