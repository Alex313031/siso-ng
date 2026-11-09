// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package e2e

import (
	"syscall"
	"testing"
)

// freePorts reserves n distinct free loopback TCP ports and returns them.
// Sockets are bound without listening and held open until all n are allocated,
// so the OS cannot hand the same port out twice.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	fds := make([]syscall.Handle, 0, n)
	ports := make([]int, 0, n)
	for range n {
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
		if err != nil {
			for _, f := range fds {
				_ = syscall.Closesocket(f)
			}
			t.Fatalf("failed to allocate socket: %v", err)
		}
		sa := &syscall.SockaddrInet4{Port: 0, Addr: [4]byte{127, 0, 0, 1}}
		if err := syscall.Bind(fd, sa); err != nil {
			_ = syscall.Closesocket(fd)
			for _, f := range fds {
				_ = syscall.Closesocket(f)
			}
			t.Fatalf("failed to bind socket: %v", err)
		}
		sa2, err := syscall.Getsockname(fd)
		if err != nil {
			_ = syscall.Closesocket(fd)
			for _, f := range fds {
				_ = syscall.Closesocket(f)
			}
			t.Fatalf("failed to getsockname: %v", err)
		}
		fds = append(fds, fd)
		ports = append(ports, sa2.(*syscall.SockaddrInet4).Port)
	}
	for _, f := range fds {
		_ = syscall.Closesocket(f)
	}
	return ports
}
