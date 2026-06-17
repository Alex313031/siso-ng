// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package collector

import "go.opentelemetry.io/collector/otelcol"

func run(params otelcol.CollectorSettings, args []string) error {
	return runInteractive(params, args)
}
