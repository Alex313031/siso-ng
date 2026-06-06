// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package mmapfile provides a thin, OS-portable wrapper for memory-mapping
// a file. Read maps a file read-only; Write truncates and maps a file
// read-write. Returned byte slices are backed by the page cache rather
// than the Go heap, so the kernel can reclaim them on memory pressure and
// callers avoid copying on read or buffering on write.
package mmapfile
