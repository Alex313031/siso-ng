// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package host

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func memoryTotal() (uint64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("invalid MemTotal line: %q", line)
			}
			val, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			// /proc/meminfo reports in kB, convert to bytes
			return val * 1024, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
