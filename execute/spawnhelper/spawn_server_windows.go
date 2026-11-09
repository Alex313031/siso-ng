// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !unix

package spawnhelper

import "flag"

// Server is spawnhelper server. needed for subcmd/spawnhelper/cmd.go
type Server struct {
	connFD  int
	logFile string
}

// RegisterFlags registers flags for server.
func (s *Server) RegisterFlags(fs *flag.FlagSet) {
	fs.IntVar(&s.connFD, "conn_fd", 0, "inherited socketpair fd to serve the spawn protocol on")
	fs.StringVar(&s.logFile, "log_file", "", "file for the helper's diagnostics (default: stderr)")
}
