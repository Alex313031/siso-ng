// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !linux && !darwin && !windows

package host

func osVersion() (string, error) {
	return "", ErrUnsupportedOS
}
