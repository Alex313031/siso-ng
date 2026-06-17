// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package proto provides protocol buffer message for profiler.
//
// It is copied from
// https://github.com/google/pprof/tree/d61513b1440d93d62aad023cc60d7a33f3917b70/proto/
// and it generates go package from the proto definition.
package proto

//go:generate ../../../scripts/install-protoc-gen-go protoc -I. --go_out=. --go_opt=paths=source_relative profile.proto
