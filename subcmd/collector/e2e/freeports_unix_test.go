// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package e2e

import (
	"net"
	"syscall"
	"testing"
)

// freePorts reserves n distinct free loopback TCP ports and returns them.
// Sockets are bound without listening and held open until all n are allocated,
// so the kernel cannot hand the same port out twice.
//
// Crucially, listen(2) is NOT called on the sockets before closing them. Calling
// listen(2) causes the Linux kernel to keep the port in the TCP listening hash
// table after close(2), causing subsequent bind(2) attempts (such as by the
// collector's listeners) to fail with EADDRINUSE ("address already in use").
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	fds := make([]int, 0, n)
	ports := make([]int, 0, n)
	for range n {
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err != nil {
			for _, f := range fds {
				_ = syscall.Close(f)
			}
			t.Fatalf("failed to allocate socket: %v", err)
		}
		sa := &syscall.SockaddrInet4{Port: 0}
		copy(sa.Addr[:], net.IPv4(127, 0, 0, 1).To4())
		if err := syscall.Bind(fd, sa); err != nil {
			_ = syscall.Close(fd)
			for _, f := range fds {
				_ = syscall.Close(f)
			}
			t.Fatalf("failed to bind socket: %v", err)
		}
		sa2, err := syscall.Getsockname(fd)
		if err != nil {
			_ = syscall.Close(fd)
			for _, f := range fds {
				_ = syscall.Close(f)
			}
			t.Fatalf("failed to getsockname: %v", err)
		}
		fds = append(fds, fd)
		ports = append(ports, sa2.(*syscall.SockaddrInet4).Port)
	}
	for _, f := range fds {
		_ = syscall.Close(f)
	}
	return ports
}
