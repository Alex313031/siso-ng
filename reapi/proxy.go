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
	"sync"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/admin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/o11y/clog"
)

// requestMetadataKey is the gRPC metadata key that carries the marshaled
// RE API RequestMetadata.
// https://github.com/bazelbuild/remote-apis/blob/8f539af4b407a4f649707f9632fc2b715c9aa065/build/bazel/remote/execution/v2/remote_execution.proto#L2034-L2045
const requestMetadataKey = "build.bazel.remote.execution.v2.requestmetadata-bin"

// traceBinKey is the gRPC metadata key that carries the caller's trace context
// in the binary format RBE understands. See otelDialOption, which is what
// writes it on a build's own RPCs.
const traceBinKey = "grpc-trace-bin"

// forwardedMetadataKeys are the incoming metadata keys the proxy copies onto
// its upstream calls. Each upstream call belongs to exactly one incoming RPC,
// so forwarding these values does not combine metadata from different callers.
// The proxy must not set Option.TracerProvider: its client would then write
// traceBinKey itself and duplicate the forwarded value.
var forwardedMetadataKeys = []string{requestMetadataKey, traceBinKey}

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

// withForwardedMetadata copies forwardedMetadataKeys from the incoming request
// onto the outgoing context, so the backend still sees the tool_invocation_id,
// target_id, action_mnemonic and friends for logging, monitoring and grouping,
// and still parents its spans under the step that issued the RPC. Without this
// the proxy would strip them: the handlers pass the server context to the
// upstream client, and gRPC does not forward incoming metadata to outgoing
// calls.
func withForwardedMetadata(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	var pairs []string
	for _, key := range forwardedMetadataKeys {
		for _, v := range md.Get(key) {
			pairs = append(pairs, key, v)
		}
	}
	if len(pairs) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}

// forwardMetadataUnaryInterceptor forwards the client's metadata upstream on
// unary RPCs.
func forwardMetadataUnaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return handler(withForwardedMetadata(ctx), req)
}

// forwardMetadataStream overrides the handler's context so streaming RPCs carry
// the forwarded metadata on upstream calls.
type forwardMetadataStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *forwardMetadataStream) Context() context.Context { return s.ctx }

// forwardMetadataStreamInterceptor forwards the client's metadata upstream on
// streaming RPCs.
func forwardMetadataStreamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(srv, &forwardMetadataStream{ServerStream: ss, ctx: withForwardedMetadata(ss.Context())})
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
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(20*1024*1024),
		grpc.ChainUnaryInterceptor(forwardMetadataUnaryInterceptor),
		grpc.ChainStreamInterceptor(forwardMetadataStreamInterceptor),
	)

	cp := &capabilitiesProxy{
		client: rpb.NewCapabilitiesClient(p.client.conn),
		resps: map[string]*rpb.ServerCapabilities{
			p.client.opt.Instance: p.client.capabilities,
		},
	}
	rpb.RegisterCapabilitiesServer(server, cp)

	ap := &actionCacheProxy{client: rpb.NewActionCacheClient(p.client.casConn)}
	rpb.RegisterActionCacheServer(server, ap)

	casp := &contentAddressableStorageProxy{
		client:            rpb.NewContentAddressableStorageClient(p.client.casConn),
		emptyDigest:       p.client.digestFn.Empty(),
		writableInstances: make(map[string]bool),
	}
	casp.checkWritable(ctx, p.client)
	rpb.RegisterContentAddressableStorageServer(server, casp)

	ep := &executionProxy{client: rpb.NewExecutionClient(p.client.conn)}
	rpb.RegisterExecutionServer(server, ep)

	op := &operationsProxy{client: longrunningpb.NewOperationsClient(p.client.conn)}
	longrunningpb.RegisterOperationsServer(server, op)

	bsp := &byteStreamProxy{client: bspb.NewByteStreamClient(p.client.casDataConn)}
	bspb.RegisterByteStreamServer(server, bsp)

	adminCleanup, err := admin.Register(server)
	if err != nil {
		return err
	}
	defer adminCleanup()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		stopped := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(stopped)
		}()

		// Give active requests 30 seconds to finish, then force close
		shutdownTimer := time.NewTimer(30 * time.Second)
		select {
		case <-stopped:
			shutdownTimer.Stop()
			fmt.Printf("gracefully shut down proxy\n")
		case <-shutdownTimer.C:
			// Forcefully stop the server if it takes too long
			server.Stop()
			fmt.Fprintf(os.Stderr, "forced shutdown of proxy\n")
		}

		return <-errCh
	case err := <-errCh:
		return err
	}
}

type capabilitiesProxy struct {
	rpb.UnimplementedCapabilitiesServer
	client rpb.CapabilitiesClient

	mu    sync.Mutex
	resps map[string]*rpb.ServerCapabilities
}

func (cp *capabilitiesProxy) GetCapabilities(ctx context.Context, req *rpb.GetCapabilitiesRequest) (*rpb.ServerCapabilities, error) {
	cp.mu.Lock()
	resp, ok := cp.resps[req.InstanceName]
	cp.mu.Unlock()
	if ok {
		clog.Infof(ctx, "cached capabilities for %q: %v", req.InstanceName, resp)
		return resp, nil
	}
	resp, err := cp.client.GetCapabilities(ctx, req)
	if err != nil {
		return resp, err
	}
	cp.mu.Lock()
	cp.resps[req.InstanceName] = resp
	cp.mu.Unlock()
	return resp, nil
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

func (ap *actionCacheProxy) AddActionLookup(ctx context.Context, req *rpb.AddActionLookupRequest) (*rpb.AddActionLookupResponse, error) {
	return ap.client.AddActionLookup(ctx, req)
}

func (ap *actionCacheProxy) ListActions(ctx context.Context, req *rpb.ListActionsRequest) (*rpb.ListActionsResponse, error) {
	return ap.client.ListActions(ctx, req)
}

type contentAddressableStorageProxy struct {
	rpb.UnimplementedContentAddressableStorageServer
	client rpb.ContentAddressableStorageClient

	// emptyDigest is the digest of the empty blob under the backend's digest
	// function, used to recognize writability probes.
	emptyDigest digest.Digest

	mu                sync.Mutex
	writableInstances map[string]bool
}

func (cp *contentAddressableStorageProxy) checkWritable(ctx context.Context, client *Client) {
	err := client.CheckWritable(ctx)
	if err != nil {
		clog.Warningf(ctx, "check writable: %v", err)
		return
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.writableInstances[client.opt.Instance] = true
	clog.Infof(ctx, "check writable %q: true", client.opt.Instance)
}

func (cp *contentAddressableStorageProxy) FindMissingBlobs(ctx context.Context, req *rpb.FindMissingBlobsRequest) (*rpb.FindMissingBlobsResponse, error) {
	return cp.client.FindMissingBlobs(ctx, req)
}

func (cp *contentAddressableStorageProxy) BatchUpdateBlobs(ctx context.Context, req *rpb.BatchUpdateBlobsRequest) (*rpb.BatchUpdateBlobsResponse, error) {
	if len(req.Requests) == 1 && digest.FromProto(req.Requests[0].GetDigest()) == cp.emptyDigest && len(req.Requests[0].GetData()) == 0 {
		cp.mu.Lock()
		ok := cp.writableInstances[req.InstanceName]
		cp.mu.Unlock()
		if ok {
			clog.Infof(ctx, "cached check writable %q: true", req.InstanceName)
			return &rpb.BatchUpdateBlobsResponse{
				Responses: []*rpb.BatchUpdateBlobsResponse_Response{
					{
						Digest: req.Requests[0].GetDigest(),
						Status: status.New(codes.OK, "").Proto(),
					},
				},
			}, nil
		}

	}
	return cp.client.BatchUpdateBlobs(ctx, req)
}

func (cp *contentAddressableStorageProxy) BatchReadBlobs(ctx context.Context, req *rpb.BatchReadBlobsRequest) (*rpb.BatchReadBlobsResponse, error) {
	return cp.client.BatchReadBlobs(ctx, req)
}

func (cp *contentAddressableStorageProxy) GetTree(req *rpb.GetTreeRequest, serv rpb.ContentAddressableStorage_GetTreeServer) error {
	gtc, err := cp.client.GetTree(serv.Context(), req)
	if err != nil {
		return err
	}
	for {
		resp, err := gtc.Recv()
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
		drainExecStream(exc)
		return nil
	}
}

// drainExecStream reads the EOF the backend sends after the done operation, so
// the upstream stream completes cleanly instead of being recorded as canceled
// when the handler returns.
func drainExecStream(exc grpc.ClientStream) {
	_ = exc.RecvMsg(&longrunningpb.Operation{})
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
		drainExecStream(exc)
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

type operationsProxy struct {
	longrunningpb.UnimplementedOperationsServer
	client longrunningpb.OperationsClient
}

func (op *operationsProxy) GetOperation(ctx context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	return op.client.GetOperation(ctx, req)
}
