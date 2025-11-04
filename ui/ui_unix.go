// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !windows

package ui

// Init initializes the stdout settings.
func Init() {}

// Restore restores the stdout settings.
func Restore() {}
