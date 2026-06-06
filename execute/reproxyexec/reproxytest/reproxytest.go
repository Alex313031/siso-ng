// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reproxytest provides fake implementation of reproxy for test.
package reproxytest

import (
	"context"
	"net"
	"os"
	"testing"

	ppb "github.com/bazelbuild/reclient/api/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Fake is fake reproxy server.
type Fake struct {
	ppb.UnimplementedCommandsServer

	RunCommandFunc func(context.Context, *ppb.RunRequest) (*ppb.RunResponse, error)
}

// RunCommand runs command on fake reproxy.
func (f Fake) RunCommand(ctx context.Context, req *ppb.RunRequest) (*ppb.RunResponse, error) {
	if f.RunCommandFunc == nil {
		return nil, status.Error(codes.Unimplemented, "")
	}
	return f.RunCommandFunc(ctx, req)
}

// Server is fake reproxy grpc server.
type Server struct {
	addr     string
	cleanups []func()
	closed   chan struct{}
}

// NewServer starts new fake reproxy grpc server.
func NewServer(ctx context.Context, t *testing.T, fake *Fake) *Server {
	t.Helper()
	s := &Server{
		closed: make(chan struct{}),
	}
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	s.cleanups = append(s.cleanups, func() { lis.Close() })

	s.addr = lis.Addr().String()
	t.Logf("fake reproxy at %s", s.addr)
	os.Setenv("RBE_server_address", s.addr)
	s.cleanups = append(s.cleanups, func() {
		os.Unsetenv("RBE_server_address")
	})

	serv := grpc.NewServer()
	ppb.RegisterCommandsServer(serv, fake)
	go func() {
		defer close(s.closed)
		err := serv.Serve(lis)
		t.Logf("Serve finished: %v", err)
	}()
	return s
}

// Addr returns address of fake reproxy server to connect to.
func (s *Server) Addr() string {
	return s.addr
}

// Close closes the server.
func (s *Server) Close() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
	s.addr = ""
	s.cleanups = nil
	<-s.closed
}
