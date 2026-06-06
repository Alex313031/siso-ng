// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package proto provides protocol buffer message for reapi.
package proto

//go:generate ../../scripts/install-protoc-gen-go protoc -I. --go_out=. --go_opt=paths=source_relative rbe_auxiliary_metadata.proto
