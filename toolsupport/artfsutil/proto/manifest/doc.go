// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package manifest provides protocol buffer message for artfs manifest.
package manifest

//go:generate ../../../../scripts/install-protoc-gen-go protoc -I. --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative manifest.proto
