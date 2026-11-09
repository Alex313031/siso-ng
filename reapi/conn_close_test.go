// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/auth/cred"
)

// fakeConn is a grpcClientConn that records Close calls. Invoke returns
// invokeErr (nil leaves the reply at its zero value, e.g. empty
// ServerCapabilities).
type fakeConn struct {
	invokeErr error
	closed    int
}

func (f *fakeConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	return f.invokeErr
}

func (f *fakeConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("fakeConn: NewStream not implemented")
}

func (f *fakeConn) Close() error {
	f.closed++
	return nil
}

func TestClose_ClosesCASConn(t *testing.T) {
	ctx := t.Context()
	conn := &fakeConn{}
	casConn := &fakeConn{}
	casDataConn := &fakeConn{}
	c, err := NewFromConn(ctx, Option{}, cred.Cred{}, conn, casConn, casDataConn)
	if err != nil {
		t.Fatalf("NewFromConn: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := conn.closed, 1; got != want {
		t.Errorf("conn.closed = %d; want %d", got, want)
	}
	if got, want := casConn.closed, 1; got != want {
		t.Errorf("casConn.closed = %d; want %d", got, want)
	}
	if got, want := casDataConn.closed, 1; got != want {
		t.Errorf("casDataConn.closed = %d; want %d", got, want)
	}
}

func TestClose_SharedConnClosedOnce(t *testing.T) {
	ctx := t.Context()
	conn := &fakeConn{}
	c, err := NewFromConn(ctx, Option{}, cred.Cred{}, conn, conn, conn)
	if err != nil {
		t.Fatalf("NewFromConn: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := conn.closed, 1; got != want {
		t.Errorf("conn.closed = %d; want %d", got, want)
	}
}

func TestNewFromConn_DigestFunctionErrorClosesBothConns(t *testing.T) {
	ctx := t.Context()
	conn := &fakeConn{}
	casConn := &fakeConn{}
	casDataConn := &fakeConn{}
	_, err := NewFromConn(ctx, Option{DigestFunction: "bogus"}, cred.Cred{}, conn, casConn, casDataConn)
	if err == nil {
		t.Fatal("NewFromConn succeeded; want digest function error")
	}
	if got, want := conn.closed, 1; got != want {
		t.Errorf("conn.closed = %d; want %d", got, want)
	}
	if got, want := casConn.closed, 1; got != want {
		t.Errorf("casConn.closed = %d; want %d", got, want)
	}
	if got, want := casDataConn.closed, 1; got != want {
		t.Errorf("casDataConn.closed = %d; want %d", got, want)
	}
}

func TestInit_CapabilitiesErrorClosesBothConns(t *testing.T) {
	ctx := t.Context()
	conn := &fakeConn{invokeErr: status.Error(codes.InvalidArgument, "no capabilities")}
	casConn := &fakeConn{}
	casDataConn := &fakeConn{}
	c, err := NewFromConn(ctx, Option{}, cred.Cred{}, conn, casConn, casDataConn)
	if err != nil {
		t.Fatalf("NewFromConn: %v", err)
	}
	if err := c.Init(ctx); err == nil {
		t.Fatal("Init succeeded; want GetCapabilities error")
	}
	if got, want := conn.closed, 1; got != want {
		t.Errorf("conn.closed = %d; want %d", got, want)
	}
	if got, want := casConn.closed, 1; got != want {
		t.Errorf("casConn.closed = %d; want %d", got, want)
	}
	if got, want := casDataConn.closed, 1; got != want {
		t.Errorf("casDataConn.closed = %d; want %d", got, want)
	}
}

func TestInit_DigestFunctionValidationErrorClosesBothConns(t *testing.T) {
	ctx := t.Context()
	// Empty ServerCapabilities advertise no digest functions, so any
	// function other than sha256 fails validation.
	conn := &fakeConn{}
	casConn := &fakeConn{}
	casDataConn := &fakeConn{}
	c, err := NewFromConn(ctx, Option{DigestFunction: "sha1"}, cred.Cred{}, conn, casConn, casDataConn)
	if err != nil {
		t.Fatalf("NewFromConn: %v", err)
	}
	if err := c.Init(ctx); err == nil {
		t.Fatal("Init succeeded; want digest function validation error")
	}
	if got, want := conn.closed, 1; got != want {
		t.Errorf("conn.closed = %d; want %d", got, want)
	}
	if got, want := casConn.closed, 1; got != want {
		t.Errorf("casConn.closed = %d; want %d", got, want)
	}
	if got, want := casDataConn.closed, 1; got != want {
		t.Errorf("casDataConn.closed = %d; want %d", got, want)
	}
}
