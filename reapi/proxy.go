// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
)

// Proxy is RE API proxy.
type Proxy struct {
	client *Client
	addr   string
}

// NewProxy creates new Proxy using client at addr.
func NewProxy(client *Client, addr string) *Proxy {
	return &Proxy{
		client: client,
		addr:   addr,
	}
}

// Serve serves RE API requests and proxies to the client.
func (p *Proxy) Serve(ctx context.Context) error {
	loc, err := url.Parse(p.addr)
	if err != nil {
		return fmt.Errorf("invalid addr %q: %w", p.addr, err)
	}
	err = os.Remove(loc.Path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove %s: %w", loc.Path, err)
	}
	lis, err := net.Listen(loc.Scheme, loc.Path)
	if err != nil {
		return fmt.Errorf("failed to listen %s %s: %w", loc.Scheme, loc.Path, err)
	}
	// TODO: set recv msg size based on server capabilities?
	server := grpc.NewServer(grpc.MaxRecvMsgSize(20 * 1024 * 1024))

	cp := &capabilitiesProxy{client: rpb.NewCapabilitiesClient(p.client.conn)}
	rpb.RegisterCapabilitiesServer(server, cp)

	ap := &actionCacheProxy{client: rpb.NewActionCacheClient(p.client.casConn)}
	rpb.RegisterActionCacheServer(server, ap)

	casp := &contentAddressableStorageProxy{client: rpb.NewContentAddressableStorageClient(p.client.casConn)}
	rpb.RegisterContentAddressableStorageServer(server, casp)

	ep := &executionProxy{client: rpb.NewExecutionClient(p.client.conn)}
	rpb.RegisterExecutionServer(server, ep)

	bsp := &byteStreamProxy{client: bspb.NewByteStreamClient(p.client.casConn)}
	bspb.RegisterByteStreamServer(server, bsp)

	return server.Serve(lis)
}

type capabilitiesProxy struct {
	rpb.UnimplementedCapabilitiesServer
	client rpb.CapabilitiesClient
}

func (cp *capabilitiesProxy) GetCapabilities(ctx context.Context, req *rpb.GetCapabilitiesRequest) (*rpb.ServerCapabilities, error) {
	return cp.client.GetCapabilities(ctx, req)
}

type actionCacheProxy struct {
	rpb.UnimplementedActionCacheServer
	client rpb.ActionCacheClient
}

func (ap *actionCacheProxy) GetActionResult(ctx context.Context, req *rpb.GetActionResultRequest) (*rpb.ActionResult, error) {
	return ap.client.GetActionResult(ctx, req)
}

func (ap *actionCacheProxy) UpdateActionResult(ctx context.Context, req *rpb.UpdateActionResultRequest) (*rpb.ActionResult, error) {
	return ap.client.UpdateActionResult(ctx, req)
}

type contentAddressableStorageProxy struct {
	rpb.UnimplementedContentAddressableStorageServer
	client rpb.ContentAddressableStorageClient
}

func (cp *contentAddressableStorageProxy) FindMissingBlobs(ctx context.Context, req *rpb.FindMissingBlobsRequest) (*rpb.FindMissingBlobsResponse, error) {
	return cp.client.FindMissingBlobs(ctx, req)
}

func (cp *contentAddressableStorageProxy) BatchUpdateBlobs(ctx context.Context, req *rpb.BatchUpdateBlobsRequest) (*rpb.BatchUpdateBlobsResponse, error) {
	return cp.client.BatchUpdateBlobs(ctx, req)
}

func (cp *contentAddressableStorageProxy) BatchReadBlobs(ctx context.Context, req *rpb.BatchReadBlobsRequest) (*rpb.BatchReadBlobsResponse, error) {
	return cp.client.BatchReadBlobs(ctx, req)
}

type executionProxy struct {
	rpb.UnimplementedExecutionServer
	client rpb.ExecutionClient
}

func (ep *executionProxy) Execute(req *rpb.ExecuteRequest, serv rpb.Execution_ExecuteServer) error {
	exc, err := ep.client.Execute(serv.Context(), req)
	if err != nil {
		return err
	}
	for {
		op, err := exc.Recv()
		if err != nil {
			return err
		}
		err = serv.Send(op)
		if err != nil {
			return err
		}
		if !op.GetDone() {
			continue
		}
		return nil
	}
}

func (ep *executionProxy) WaitExecution(req *rpb.WaitExecutionRequest, serv rpb.Execution_WaitExecutionServer) error {
	exc, err := ep.client.WaitExecution(serv.Context(), req)
	if err != nil {
		return err
	}
	for {
		op, err := exc.Recv()
		if err != nil {
			return err
		}
		err = serv.Send(op)
		if err != nil {
			return err
		}
		if !op.GetDone() {
			continue
		}
		return nil
	}
}

type byteStreamProxy struct {
	bspb.UnimplementedByteStreamServer
	client bspb.ByteStreamClient
}

func (bp *byteStreamProxy) Read(req *bspb.ReadRequest, serv bspb.ByteStream_ReadServer) error {
	bsc, err := bp.client.Read(serv.Context(), req)
	if err != nil {
		return err
	}
	for {
		resp, err := bsc.Recv()
		if err == io.EOF { //nolint:errorlint
			return nil
		}
		if err != nil {
			return err
		}
		err = serv.Send(resp)
		if err != nil {
			return err
		}
	}
}

func (bp *byteStreamProxy) Write(serv bspb.ByteStream_WriteServer) error {
	bsc, err := bp.client.Write(serv.Context())
	if err != nil {
		return err
	}
	for {
		wreq, err := serv.Recv()
		if err == io.EOF { //nolint:errorlint
			resp, err := bsc.CloseAndRecv()
			if err != nil {
				return err
			}
			return serv.SendAndClose(resp)
		}
		if err != nil {
			return err
		}
		err = bsc.Send(wreq)
		if err != nil {
			return err
		}
	}
}
