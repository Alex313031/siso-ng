// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package runtimex

func getproccount() int {
	// Use the value from runtime.NumCPU() instead.
	return 0
}
