// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package mwc provides embed.FS containing Material Web Components and dependencies.
package mwc

import (
	"embed"
	"io/fs"
)

var (
	//go:embed components-chromium/*
	embedFS embed.FS
	// NodeModulesFS is embed.FS containing Material Web Components node module and dependency node modules.
	NodeModulesFS, _ = fs.Sub(embedFS, "components-chromium/node_modules")
)
