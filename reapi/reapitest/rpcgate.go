// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapitest

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc"
)

// RPCGate holds client RPCs of one method (identified by its full method
// name, e.g. rpb.ContentAddressableStorage_BatchUpdateBlobs_FullMethodName)
// until Release is called, so tests can observe behavior while such an
// RPC is in flight. Held RPCs still honor their context's cancellation.
type RPCGate struct {
	method      string
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

// NewRPCGate returns a gate for the RPC with the given full method name.
func NewRPCGate(fullMethodName string) *RPCGate {
	return &RPCGate{
		method:  fullMethodName,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// DialOption returns the grpc.DialOption that installs the gate.
func (g *RPCGate) DialOption() grpc.DialOption {
	return grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if err := g.hold(ctx, method); err != nil {
			return err
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	})
}

func (g *RPCGate) hold(ctx context.Context, method string) error {
	if method != g.method {
		return nil
	}
	g.enteredOnce.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// WaitEntered waits until the first gated RPC has arrived, or ctx is done.
func (g *RPCGate) WaitEntered(ctx context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatalf("no %s RPC arrived at the gate: %v", g.method, context.Cause(ctx))
	}
}

// Release lets gated RPCs proceed. It is safe to call more than once,
// concurrently.
func (g *RPCGate) Release() {
	g.releaseOnce.Do(func() { close(g.release) })
}
