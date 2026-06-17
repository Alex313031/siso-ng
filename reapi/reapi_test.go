// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

type fakeCAS struct {
	rpb.UnimplementedContentAddressableStorageServer
	t    *testing.T
	code codes.Code
	// remaining number to reply error with code.
	n int
}

func (f *fakeCAS) BatchUpdateBlobs(ctx context.Context, req *rpb.BatchUpdateBlobsRequest) (*rpb.BatchUpdateBlobsResponse, error) {
	n := f.n
	f.n--
	var err error
	if n > 0 {
		err = status.Error(f.code, "error")
	}
	f.t.Logf("n=%d: err=%v", n, err)
	return &rpb.BatchUpdateBlobsResponse{}, err
}

func TestServiceConfig_CAS(t *testing.T) {
	for _, tc := range []struct {
		code     codes.Code
		n        int
		wantCode codes.Code
	}{
		{
			code:     codes.OK,
			n:        0,
			wantCode: codes.OK,
		},
		{
			code:     codes.Unavailable,
			n:        1,
			wantCode: codes.OK,
		},
		{
			code:     codes.ResourceExhausted,
			n:        1,
			wantCode: codes.OK,
		},
		{
			code:     codes.Unavailable,
			n:        6,
			wantCode: codes.Unavailable,
		},
		{
			code:     codes.ResourceExhausted,
			n:        6,
			wantCode: codes.ResourceExhausted,
		},
		{
			code:     codes.PermissionDenied,
			n:        1,
			wantCode: codes.PermissionDenied,
		},
		{
			code:     codes.Internal,
			n:        1,
			wantCode: codes.OK,
		},
		{
			code:     codes.Unknown,
			n:        1,
			wantCode: codes.OK,
		},
		{
			code:     codes.Aborted,
			n:        1,
			wantCode: codes.OK,
		},
	} {
		t.Run(fmt.Sprintf("%s_%d", tc.code, tc.n), func(t *testing.T) {
			ctx := t.Context()
			ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()

			ch := make(chan struct{})
			var lc net.ListenConfig
			lis, err := lc.Listen(ctx, "tcp", "localhost:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				err = lis.Close()
				if err != nil {
					t.Error(err)
				}
				<-ch
			})
			addr := lis.Addr().String()
			t.Logf("fake addr: %s", addr)
			serv := grpc.NewServer()
			cas := &fakeCAS{
				t:    t,
				code: tc.code,
				n:    tc.n,
			}
			rpb.RegisterContentAddressableStorageServer(serv, cas)
			reflection.Register(serv)
			go func() {
				defer close(ch)
				err := serv.Serve(lis)
				t.Logf("serve finished: %v", err)
			}()

			keepAliveParams := keepalive.ClientParameters{
				Time:    30 * time.Second,
				Timeout: 20 * time.Second,
			}
			conn, err := grpc.NewClient(addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, reapi.DialOptions(keepAliveParams)...)...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				err := conn.Close()
				if err != nil {
					t.Errorf("conn close=%v", err)
				}
			}()
			t.Logf("dial done")
			client := rpb.NewContentAddressableStorageClient(conn)
			_, err = client.BatchUpdateBlobs(ctx, &rpb.BatchUpdateBlobsRequest{})
			if status.Code(err) != tc.wantCode {
				t.Errorf("BatchUpdateBlobs=%v; want %v", err, tc.wantCode)
			}
		})
	}
}

func TestUploadAll(t *testing.T) {
	ctx := t.Context()
	fakere := &reapitest.Fake{}
	cl := reapitest.New(ctx, t, fakere)
	ds := digest.NewStore()

	// No uploads
	n, err := cl.UploadAll(ctx, ds)
	if err != nil || n != 0 {
		t.Errorf("UploadAll()=%d,%v: want 0,nil", n, err)
	}

	// Upload missing blobs.
	// The small blob will be uploaded by BatchUpdateBlobs RPC
	smallBlob := []byte("foo")
	sd := digest.FromBytes("small", smallBlob)
	ds.Set(sd)
	// The large blob will be uploaded by ByteStream RPC
	pattern := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A}
	var buf bytes.Buffer
	for buf.Len() < 10*1024*1024 {
		buf.WriteByte(pattern[rand.Intn(len(pattern))])
	}
	largeBlob := buf.Bytes()
	ld := digest.FromBytes("large", largeBlob)
	ds.Set(ld)
	n, err = cl.UploadAll(ctx, ds)
	if err != nil || n != 2 {
		t.Fatalf("UploadAll()=%d,%v: want 2,nil", n, err)
	}
	// Download the small blob with BatchReadBlobs RPC
	b, err := cl.Get(ctx, sd.Digest(), sd.String())
	if !bytes.Equal(b, smallBlob) || err != nil {
		t.Errorf("cl.Get()=%b,%v: want %v,nil", b, err, smallBlob)
	}
	// Download the large blob with ByteStream RPC
	b, err = cl.Get(ctx, ld.Digest(), ld.String())
	if !bytes.Equal(b, largeBlob) || err != nil {
		t.Errorf("cl.Get()=_,%v: want _,nil", err)
	}
}

// TODO: Record REAPI calls on reapitest.Fake and verify that the requests
// are sent as expected.
func TestUploadAllWithCompression(t *testing.T) {
	ctx := t.Context()
	fakere := &reapitest.Fake{}
	opt := reapi.Option{
		CompressedBlob: 1,
	}
	cl := reapitest.NewWithOption(ctx, t, fakere, opt)
	ds := digest.NewStore()

	// No uploads
	n, err := cl.UploadAll(ctx, ds)
	if err != nil || n != 0 {
		t.Errorf("UploadAll()=%d,%v: want 0,nil", n, err)
	}

	// Upload missing blobs.
	// The small blob will be uploaded by BatchUpdateBlobs RPC
	smallBlob := []byte("foo")
	sd := digest.FromBytes("small", smallBlob)
	ds.Set(sd)
	// The large blob will be uploaded by ByteStream RPC
	pattern := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A}
	var buf bytes.Buffer
	for buf.Len() < 10*1024*1024 {
		buf.WriteByte(pattern[rand.Intn(len(pattern))])
	}
	largeBlob := buf.Bytes()
	ld := digest.FromBytes("large", largeBlob)
	ds.Set(ld)
	n, err = cl.UploadAll(ctx, ds)
	if err != nil || n != 2 {
		t.Fatalf("UploadAll()=%d,%v: want 2,nil", n, err)
	}
	// Download the small blob with BatchReadBlobs RPC
	b, err := cl.Get(ctx, sd.Digest(), sd.String())
	if !bytes.Equal(b, smallBlob) || err != nil {
		t.Errorf("cl.Get()=%b,%v: want %v,nil", b, err, smallBlob)
	}
	// Download the large blob with ByteStream RPC
	b, err = cl.Get(ctx, ld.Digest(), ld.String())
	if !bytes.Equal(b, largeBlob) || err != nil {
		t.Errorf("cl.Get()=_,%v: want _,nil", err)
	}
}

// canceledCounter is a minimal grpc stats handler that counts RPCs
// completing with codes.Canceled, without depending on any
// siso-specific metrics infrastructure.
type canceledCounter struct {
	canceled atomic.Int64
	wg       sync.WaitGroup
}

func (c *canceledCounter) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	c.wg.Add(1)
	return ctx
}
func (c *canceledCounter) HandleRPC(_ context.Context, s stats.RPCStats) {
	if end, ok := s.(*stats.End); ok {
		if end.Error != nil && status.Code(end.Error) == codes.Canceled {
			c.canceled.Add(1)
		}
		c.wg.Done()
	}
}
func (c *canceledCounter) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (c *canceledCounter) HandleConn(context.Context, stats.ConnStats) {}

// fakeExecServer sends one pending op then one done op.
// Used by TestExecuteStream_NoCanceledOnSuccess.
type fakeExecServer struct {
	rpb.UnimplementedExecutionServer
}

func (s *fakeExecServer) Execute(_ *rpb.ExecuteRequest, stream rpb.Execution_ExecuteServer) error {
	mdAny, _ := anypb.New(&rpb.ExecuteOperationMetadata{
		Stage: rpb.ExecutionStage_QUEUED,
	})
	if err := stream.Send(&longrunningpb.Operation{
		Name:     "op/1",
		Done:     false,
		Metadata: mdAny,
	}); err != nil {
		return err
	}
	respAny, _ := anypb.New(&rpb.ExecuteResponse{
		Result: &rpb.ActionResult{ExitCode: 0},
	})
	return stream.Send(&longrunningpb.Operation{
		Name:   "op/1",
		Done:   true,
		Result: &longrunningpb.Operation_Response{Response: respAny},
	})
}

// TestExecuteStream_NoCanceledOnSuccess verifies that a successful
// Execute call does not report Canceled to the server. Without the
// stream drain, the caller's deferred context cancel tears the stream
// down before grpc observes the server's trailing EOF.
func TestExecuteStream_NoCanceledOnSuccess(t *testing.T) {
	ctx := t.Context()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	rpb.RegisterExecutionServer(srv, &fakeExecServer{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	cc := &canceledCounter{}
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(cc),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	cl, err := reapi.NewFromConn(ctx, reapi.Option{
		Instance:       "test",
		KeepExecStream: true,
	}, cred.Cred{}, conn, conn)
	if err != nil {
		t.Fatal(err)
	}

	execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, _, err = cl.ExecuteAndWait(execCtx, &rpb.ExecuteRequest{
		ActionDigest:    &rpb.Digest{Hash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		SkipCacheLookup: true,
	})
	// Cancel after the call returns. Without the drain fix, this
	// tears down the stream as Canceled.
	cancel()
	if err != nil {
		t.Fatalf("ExecuteAndWait: %v", err)
	}

	cc.wg.Wait()

	if got := cc.canceled.Load(); got != 0 {
		t.Errorf("Execute completed with %d Canceled errors, want 0", got)
	}
}

// TestByteStreamRead_NoCanceledOnSuccess verifies that a successful
// ByteStream.Read (via Client.Get on a blob routed through ByteStream)
// does not report Canceled to the server. Without the stream drain,
// the context cancel after io.ReadFull tears the stream down before
// grpc observes EOF.
func TestByteStreamRead_NoCanceledOnSuccess(t *testing.T) {
	ctx := t.Context()

	cc := &canceledCounter{}
	cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{
		ByteStreamReadThreshold: 1,
	}, grpc.WithStatsHandler(cc))

	blob := []byte("test blob for bytestream read")
	ds := digest.NewStore()
	d := digest.FromBytes("test-blob", blob)
	ds.Set(d)
	if _, err := cl.UploadAll(ctx, ds); err != nil {
		t.Fatalf("UploadAll: %v", err)
	}

	got, err := cl.Get(ctx, d.Digest(), "test-blob")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("Get returned wrong data")
	}

	cc.wg.Wait()

	if got := cc.canceled.Load(); got != 0 {
		t.Errorf("ByteStream.Read completed with %d Canceled errors, want 0", got)
	}
}
