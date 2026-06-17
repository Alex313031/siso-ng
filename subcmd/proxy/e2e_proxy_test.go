// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !windows

package proxy_test

import (
	"context"
	"flag"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/subcmd/proxy"
)

type dummyCapabilities struct {
	rpb.UnimplementedCapabilitiesServer
}

func (dummyCapabilities) GetCapabilities(context.Context, *rpb.GetCapabilitiesRequest) (*rpb.ServerCapabilities, error) {
	return &rpb.ServerCapabilities{}, nil
}

func TestProxyCleanShutdownOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	rpb.RegisterCapabilitiesServer(grpcServer, dummyCapabilities{})
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	cmd := proxy.Cmd(cred.Options{})
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	cmd.SetFlags(fs)

	sockPath := filepath.Join(t.TempDir(), "proxy.sock")
	if len(sockPath) > 100 {
		// MacOS limits unix socket paths to 104 characters.
		// Use a shorter path if TempDir is too deep.
		sockPath = filepath.Join("/tmp", "siso-proxy-test-"+t.Name()+".sock")
	}
	addr := "unix://" + sockPath

	err = fs.Parse([]string{
		"-reapi_address", lis.Addr().String(),
		"-reapi_instance", "projects/test/instances/default",
		"-reapi_insecure=true",
		"-addr", addr,
	})
	if err != nil {
		t.Fatal(err)
	}

	cmdCtx, cmdCancel := context.WithCancel(ctx)
	defer cmdCancel()

	done := make(chan struct{})
	go func() {
		_ = cmd.Execute(cmdCtx, fs)
		close(done)
	}()

	// Wait for the proxy to start serving (socket file created)
	for range 50 {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatal("proxy didn't create socket in time")
	}

	// Cancel the context, which should cause Execute to return
	cmdCancel()

	select {
	case <-done:
		// Success! Proxy stopped cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not shut down cleanly after context cancellation")
	}
}
