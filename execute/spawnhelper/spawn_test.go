// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package spawnhelper

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	epb "go.chromium.org/build/siso/execute/proto"
)

// TestMain lets the test binary act as the spawn helper when re-exec'd by launch,
// so runViaHelper exercises the real cross-process path.
func TestMain(m *testing.M) {
	for i, a := range os.Args {
		if a != "spawn-helper" {
			continue
		}
		fs := flag.NewFlagSet("spawn-helper", flag.ContinueOnError)
		var server Server
		server.RegisterFlags(fs)
		fs.Parse(os.Args[i+1:])
		_ = server.Serve(context.Background(), testSpawner{})
		os.Exit(0)
	}
	m.Run()
}

type testSpawner struct{}

func (testSpawner) Spawn(ctx context.Context, req *epb.SpawnRequest) (*epb.SpawnResult, error) {
	if len(req.Args) > 0 && req.Args[0] == "test-dial" {
		addr := req.Args[1]
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dial failed: %v", err)
		}
		conn.Close()
		return &epb.SpawnResult{}, nil
	}
	return nil, fmt.Errorf("not implemented")
}

// TestLaunchSetsOwnProcessGroup verifies launch puts the helper in its own process
// group, so a terminal Ctrl-C or group SIGTERM to siso doesn't reach it directly.
func TestLaunchSetsOwnProcessGroup(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Launch([]string{exe, "spawn-helper"}, "", false)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() {
		_ = c.conn.close()
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	})

	pid := c.cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", pid, err)
	}
	if pgid != pid {
		t.Errorf("helper pgid = %d, want it to lead its own group (== pid %d)", pgid, pid)
	}
	if self, err := syscall.Getpgid(0); err == nil && pgid == self {
		t.Errorf("helper pgid = %d shares the test process group %d; want its own", pgid, self)
	}
}
