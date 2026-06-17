// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package metricscmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"go.chromium.org/build/siso/build"
)

func loadMetrics(fname string) ([]build.StepMetric, error) {
	f, err := os.Open(fname)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	var metrics []build.StepMetric
	for {
		var m build.StepMetric
		err := d.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse error in %s:%d: %w", fname, d.InputOffset(), err)
		}
		metrics = append(metrics, m)
	}
	return metrics, nil
}
