// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package reproxyexec

import (
	"context"
	"net"
	"strings"

	winio "github.com/Microsoft/go-winio"
	"google.golang.org/grpc"
)

// dialContext connects to the serverAddress for grpc.
// if serverAddr is `pipe://<addr>`, it connects to named pipe (`\\.\\pipe\<addr>`).
func dialContext(ctx context.Context, serverAddr string) (*grpc.ClientConn, error) {
	if strings.HasPrefix(serverAddr, "pipe://") {
		return dialPipe(ctx, strings.TrimPrefix(serverAddr, "pipe://"))
	}
	return grpc.NewClient(
		serverAddr,
		grpc.WithInsecure(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcMaxMsgSize)))
}

func dialPipe(ctx context.Context, pipe string) (*grpc.ClientConn, error) {
	addr := `\\.\pipe\` + pipe
	return grpc.NewClient(
		"passthrough:"+addr,
		grpc.WithInsecure(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcMaxMsgSize)),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, addr)
		}))
}
