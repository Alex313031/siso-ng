// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"errors"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
)

type executeOperationConn struct {
	callOpts []grpc.CallOption
}

func (c *executeOperationConn) Invoke(_ context.Context, _ string, _, reply any, opts ...grpc.CallOption) error {
	c.callOpts = opts
	reply.(*longrunningpb.Operation).Done = true
	return nil
}

func (*executeOperationConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("executeOperationConn: NewStream not implemented")
}

func (*executeOperationConn) Close() error { return nil }

func TestExecuteOperationMarksGetOperationStatic(t *testing.T) {
	conn := &executeOperationConn{}
	c := &Client{conn: conn}
	if _, err := c.executeOperation(t.Context(), "operations/test"); err != nil {
		t.Fatalf("executeOperation() failed: %v", err)
	}
	for _, opt := range conn.callOpts {
		if _, ok := opt.(grpc.StaticMethodCallOption); ok {
			return
		}
	}
	t.Error("executeOperation() did not pass grpc.StaticMethod()")
}
