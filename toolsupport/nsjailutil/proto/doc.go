// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package proto provides protocol buffer message for nsjail.
// nsjail proto can be found at https://github.com/google/nsjail
// or googleplex-android's external/platform/nsjail.
//
// To update, copy config.proto from googleprox-android's
// external/platform/nsjail repo, and add copyright header,
// then run `go generate .' here.
package proto

//go:generate ../../../scripts/install-protoc-gen-go protoc -I. -I.. --go_out=. --go_opt=Mconfig.proto=go.chromium.org/build/siso/toolsupports/nsjail/proto --go_opt=paths=source_relative config.proto
