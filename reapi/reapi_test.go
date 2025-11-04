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
	"testing"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

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
