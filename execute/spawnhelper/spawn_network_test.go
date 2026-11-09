// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package spawnhelper

import (
	"context"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	epb "go.chromium.org/build/siso/execute/proto"
)

// TestLaunchWithNetworkBlocked verifies that Launch successfully returns a running helper
// when the blockNetwork parameter is true. It verifies isolation by ensuring
// the child cannot talk to a server on the parent's loopback interface.
func TestLaunchWithNetworkBlocked(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// 1. Setup host listener
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on host localhost: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()

	// 2. Launch child WITH network blocked
	c, err := Launch([]string{exe, "spawn-helper"}, "", true)
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatalf("Launch with blockNetwork=true on non-linux succeeded, expected error")
		}
		return
	}
	if err != nil {
		t.Fatalf("launch with network blocked: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = c.Wait()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 3. Ask child to connect to the host's localhost listener
	_, err = c.Run(ctx, &epb.SpawnRequest{
		Args: []string{"test-dial", addr},
	})

	// Because blockNetwork=true isolates loopback, it should NOT be able to connect!
	if err == nil {
		t.Errorf("Child connected to isolated host localhost! Expected connection failure, but dial succeeded.")
	}
}

// TestLaunchWithNetworkAllowed verifies that when blockNetwork is false, the
// child *can* connect to a server on the host's loopback.
func TestLaunchWithNetworkAllowed(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// 1. Setup host listener
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on host localhost: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()

	// Accept immediately in a goroutine
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Close()
		}
	}()

	// 2. Launch child without blocking networks
	c, err := Launch([]string{exe, "spawn-helper"}, "", false)
	if err != nil {
		t.Fatalf("launch with network allowed: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = c.Wait()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 3. Ask child to connect to the host's localhost listener
	_, err = c.Run(ctx, &epb.SpawnRequest{
		Args: []string{"test-dial", addr},
	})
	if err != nil {
		t.Fatalf("Child could NOT connect to host localhost! Expected success, but got error: %v", err)
	}
}
