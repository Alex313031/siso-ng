// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package server provides protocol buffer message for cartfs server.
package server

// cartfs.proto imports proto/server/tap.proto and protoc produces
// source path based init func (e.g. file_proto_server_tap_proto_init).
// to match it between cartfs.proto and tap.proto, need to use the
// same relative import path (i.e. proto/server/*.proto from ../..
// with using -I.).
//go:generate ./protoc-gen.sh
