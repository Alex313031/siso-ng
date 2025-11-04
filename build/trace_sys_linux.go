// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package build

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"go.chromium.org/build/siso/o11y/clog"
)

type sysRecord struct {
	// https://docs.kernel.org/accounting/psi.html
	// pressure stall information
	// psiMemory is PSI memory `some` total value.
	psiMemory int64
	start     time.Time
}

func readProcPressureMemorySome() (int64, error) {
	buf, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0, err
	}
	return parseProcPressureMemorySome(buf)
}

func parseProcPressureMemorySome(buf []byte) (int64, error) {
	// parse `total` value on `some` line
	//  some avg10=0.00 avg60=0.00 avg300=0.00 total=10270662
	//  full avg10=0.00 avg60=0.00 avg300=0.00 total=9928704
	for len(buf) > 0 {
		line := buf
		i := bytes.IndexByte(line, '\n')
		if i >= 0 {
			buf = line[i+1:]
			line = line[:i]
		} else {
			buf = nil
		}
		if !bytes.HasPrefix(line, []byte("some ")) {
			continue
		}
		i = bytes.Index(line, []byte("total="))
		if i < 0 {
			continue
		}
		value := bytes.TrimPrefix(line[i:], []byte("total="))
		n, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return n, fmt.Errorf("failed to parse %q: %w", value, err)
		}
		return n, nil
	}
	return 0, io.EOF
}

func (s *sysRecord) get(ctx context.Context) {
	var err error
	s.psiMemory, err = readProcPressureMemorySome()
	if err != nil {
		clog.Warningf(ctx, "failed to read /proc/pressure/memory: %v", err)
	}
}

func (s *sysRecord) sample(ctx context.Context, t time.Time) []traceEventObject {
	psiMemory, err := readProcPressureMemorySome()
	if err != nil {
		clog.Warningf(ctx, "failed to read /proc/pressure/memory: %v", err)
		return nil
	}
	m := psiMemory - s.psiMemory
	// https://docs.kernel.org/accounting/psi.html
	// total line is the total absolute stall time (in us).
	// note sample is called every second
	switch {
	case m > 500*1000: // 500ms stalled
		clog.Warningf(ctx, "memory stall %s/s", time.Duration(m*1000))
	case m > 100*1000: // 100ms stalled
		clog.Infof(ctx, "memory stall %s/s", time.Duration(m*1000))
	}
	s.psiMemory = psiMemory
	return []traceEventObject{
		{
			Ph:   "C",
			T:    t.Sub(s.start).Microseconds(),
			Pid:  sysPid,
			Tid:  sysPid,
			Name: "pressure",
			Args: map[string]any{
				"memory/some": m,
			},
		},
	}
}
