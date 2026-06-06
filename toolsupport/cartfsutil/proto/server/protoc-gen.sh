#!/bin/sh
# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.
cd ../.. && ../../scripts/install-protoc-gen-go protoc -I. -I../.. --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/server/cartfs.proto proto/server/tap.proto

