// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// A blocked wait in these tests is a test failure that surfaces as a
// hang; it is bounded by `go test -timeout`.

// TestUploadAll_RequesterCancelDoesNotCancelUpload checks that canceling
// the requester's context makes UploadAll return promptly while the
// upload operation itself keeps running to completion, so that a later
// request for the same blob is served by that operation. b/565765743
func TestUploadAll_RequesterCancelDoesNotCancelUpload(t *testing.T) {
	ctx := t.Context()
	gate := reapitest.NewRPCGate(rpb.ContentAddressableStorage_BatchUpdateBlobs_FullMethodName)
	t.Cleanup(gate.Release)
	cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{}, gate.DialOption())

	ds := blob.NewStore()
	d := blob.FromBytes(digest.SHA256, "blob", []byte("detached upload"))
	ds.Set(d)

	// Requester 1: canceled while its upload is held in the gate.
	reqCtx, reqCancel := context.WithCancelCause(ctx)
	errCh := make(chan error, 1)
	go func() {
		_, err := cl.UploadAll(reqCtx, ds)
		errCh <- err
	}()
	gate.WaitEntered(ctx, t)
	wantCause := errors.New("local racer won")
	reqCancel(wantCause)
	if err := <-errCh; !errors.Is(err, wantCause) {
		t.Errorf("UploadAll(canceled)=%v; want cause %v", err, wantCause)
	}

	// Requester 2 for the same blob: waits on the pending upload
	// operation, which must still complete once the gate opens.
	errCh2 := make(chan error, 1)
	var n2 int
	go func() {
		var err error
		n2, err = cl.UploadAll(ctx, ds)
		errCh2 <- err
	}()
	gate.Release()
	if err := <-errCh2; err != nil {
		t.Fatalf("UploadAll(pending)=%d,%v; want _,nil (requester 1's cancel must not poison the shared upload)", n2, err)
	}
	// The blob was uploaded by requester 1's operation, not this one.
	if n2 != 0 {
		t.Errorf("UploadAll(pending) uploaded %d; want 0", n2)
	}

	// And the blob is actually in the CAS.
	got, err := cl.Get(ctx, d.Digest(), "blob")
	if err != nil || string(got) != "detached upload" {
		t.Errorf("Get=%q,%v; want %q,nil", got, err, "detached upload")
	}
}

// TestUploadAll_CloseCancelsUpload checks that Close cancels in-flight
// upload operations and releases their waiters.
func TestUploadAll_CloseCancelsUpload(t *testing.T) {
	ctx := t.Context()
	gate := reapitest.NewRPCGate(rpb.ContentAddressableStorage_BatchUpdateBlobs_FullMethodName)
	t.Cleanup(gate.Release)
	cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{}, gate.DialOption())

	ds := blob.NewStore()
	ds.Set(blob.FromBytes(digest.SHA256, "blob", []byte("canceled by close")))

	errCh := make(chan error, 1)
	go func() {
		_, err := cl.UploadAll(ctx, ds)
		errCh <- err
	}()
	gate.WaitEntered(ctx, t)

	closed := make(chan error, 1)
	go func() { closed <- cl.Close() }()
	if err := <-errCh; !errors.Is(err, reapi.ErrClientClosed) {
		t.Errorf("UploadAll=%v after Close; want %v", err, reapi.ErrClientClosed)
	}
	<-closed
}

// TestUploadAll_QueuedRequestsCompleteAndCloseFailsThem checks the worker
// pool: with a single worker held in the gate, further requests queue up
// and are served in turn once the worker is free; requests still queued
// when the client is closed fail with the close cause instead of leaving
// their waiters hanging.
func TestUploadAll_QueuedRequestsCompleteAndCloseFailsThem(t *testing.T) {
	ctx := t.Context()
	gate := reapitest.NewRPCGate(rpb.ContentAddressableStorage_BatchUpdateBlobs_FullMethodName)
	t.Cleanup(gate.Release)
	cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{
		UploadLimit: 1,
	}, gate.DialOption())

	newStore := func(name string) *blob.Store {
		ds := blob.NewStore()
		ds.Set(blob.FromBytes(digest.SHA256, name, []byte("content of "+name)))
		return ds
	}
	type result struct {
		n   int
		err error
	}
	upload := func(cl *reapi.Client, ds *blob.Store) <-chan result {
		ch := make(chan result, 1)
		go func() {
			n, err := cl.UploadAll(ctx, ds)
			ch <- result{n, err}
		}()
		return ch
	}

	// Request 1 occupies the only worker; request 2 queues behind it.
	r1 := upload(cl, newStore("one"))
	gate.WaitEntered(ctx, t)
	r2 := upload(cl, newStore("two"))

	// Let the worker through: both requests complete, in order.
	gate.Release()
	for i, ch := range []<-chan result{r1, r2} {
		if r := <-ch; r.err != nil || r.n != 1 {
			t.Errorf("request %d: UploadAll=%d,%v; want 1,nil", i+1, r.n, r.err)
		}
	}

	// Hold the worker again, queue a request behind it, then close:
	// the held request fails (canceled RPC) and the queued one fails
	// with the close cause; neither waiter is left hanging.
	gate2 := reapitest.NewRPCGate(rpb.ContentAddressableStorage_FindMissingBlobs_FullMethodName)
	t.Cleanup(gate2.Release)
	cl2 := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{
		UploadLimit: 1,
	}, gate2.DialOption())
	held := upload(cl2, newStore("held"))
	gate2.WaitEntered(ctx, t)
	queued := upload(cl2, newStore("queued"))
	closed := make(chan error, 1)
	go func() { closed <- cl2.Close() }()
	for name, ch := range map[string]<-chan result{"held": held, "queued": queued} {
		if r := <-ch; !errors.Is(r.err, reapi.ErrClientClosed) {
			t.Errorf("%s request: UploadAll=%d,%v after Close; want %v", name, r.n, r.err, reapi.ErrClientClosed)
		}
	}
	<-closed
}

type outPayloadHandler struct {
	method  string
	onSent  func()
	release chan struct{}
	once    sync.Once
}

type rpcMethodKey struct{}

func (h *outPayloadHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, rpcMethodKey{}, info.FullMethodName)
}

func (h *outPayloadHandler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	if _, ok := rs.(*stats.OutPayload); ok {
		if m, ok := ctx.Value(rpcMethodKey{}).(string); ok && m == h.method {
			h.once.Do(func() {
				h.onSent()
				<-h.release
			})
		}
	}
}

func (h *outPayloadHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h *outPayloadHandler) HandleConn(context.Context, stats.ConnStats) {}

// TestUploadAll_ServerShutdownWaitsForInFlightHandler checks that shutting
// down a reapitest server while a detached BatchUpdateBlobs RPC is still
// writing blobs into its CAS dir waits for the handler to finish before
// t.TempDir's RemoveAll cleanup runs. b/575196094
func TestUploadAll_ServerShutdownWaitsForInFlightHandler(t *testing.T) {
	ctx := t.Context()
	reqCtx, reqCancel := context.WithCancel(ctx)
	defer reqCancel()
	sh := &outPayloadHandler{
		method:  rpb.ContentAddressableStorage_BatchUpdateBlobs_FullMethodName,
		onSent:  reqCancel,
		release: make(chan struct{}),
	}
	defer close(sh.release)
	cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{}, grpc.WithStatsHandler(sh))
	t.Cleanup(func() {
		if err := cl.Close(); err != nil {
			t.Error(err)
		}
	})

	ds := blob.NewStore()
	for i := range 200 {
		name := fmt.Sprintf("blob-%d", i)
		ds.Set(blob.FromBytes(digest.SHA256, name, []byte(name)))
	}

	// Cancel reqCtx and hold the client goroutine inside OutPayload (after
	// the request frame has been queued to the transport) until UploadAll
	// observes the cancellation, so UploadAll deterministically returns
	// context.Canceled while the server handler is writing blobs.
	if _, err := cl.UploadAll(reqCtx, ds); !errors.Is(err, context.Canceled) {
		t.Errorf("UploadAll(canceled)=%v; want %v", err, context.Canceled)
	}
}
