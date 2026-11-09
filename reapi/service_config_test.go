// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"
)

func TestDialOptionsOmitZeroKeepalive(t *testing.T) {
	if got, want := len(keepaliveDialOptions(keepalive.ClientParameters{})), 0; got != want {
		t.Errorf("len(keepaliveDialOptions(zero keepalive)) = %d; want %d", got, want)
	}
	params := keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second}
	if got, want := len(keepaliveDialOptions(params)), 1; got != want {
		t.Errorf("len(keepaliveDialOptions(configured keepalive)) = %d; want %d", got, want)
	}
}

// flakyActionCache counts GetActionResult attempts and lets the test decide
// what each attempt returns. Retries are transparent to the client, so the
// attempt count is only observable here on the server.
type flakyActionCache struct {
	rpb.UnimplementedActionCacheServer

	// reply returns the error for attempt n (1-based); nil means success.
	reply func(n int) error

	mu       sync.Mutex
	attempts int
}

func (f *flakyActionCache) GetActionResult(_ context.Context, _ *rpb.GetActionResultRequest) (*rpb.ActionResult, error) {
	f.mu.Lock()
	f.attempts++
	n := f.attempts
	f.mu.Unlock()
	if err := f.reply(n); err != nil {
		return nil, err
	}
	return &rpb.ActionResult{ExitCode: 0}, nil
}

func (f *flakyActionCache) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// dialWithServiceConfig serves cache over bufconn and dials it with the real
// RE API dial options, so the connection carries the production
// service_config.json and nothing else retries on top of it.
func dialWithServiceConfig(t *testing.T, cache rpb.ActionCacheServer) rpb.ActionCacheClient {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	rpb.RegisterActionCacheServer(srv, cache)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	dopts := append(DialOptions(keepalive.ClientParameters{}),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///bufnet", dopts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return rpb.NewActionCacheClient(conn)
}

// TestServiceConfigRetriesGetActionResult documents what the gRPC method
// config in service_config.json actually does for GetActionResult, measured
// against a real gRPC server. The client makes ONE call in each case; every
// additional attempt the server sees came from gRPC itself.
func TestServiceConfigRetriesGetActionResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		// reply drives the fake server.
		reply func(n int) error
		// wantAttempts is how many times the server should be hit for a
		// single client call.
		wantAttempts int
		wantCode     codes.Code
	}{
		{
			// The retriable codes are retried transparently: the caller
			// never sees the failures.
			name: "UnavailableThenOK",
			reply: func(n int) error {
				if n <= 2 {
					return status.Error(codes.Unavailable, "backend unavailable")
				}
				return nil
			},
			wantAttempts: 3,
			wantCode:     codes.OK,
		},
		{
			// maxAttempts includes the original call, so a permanently
			// failing backend is tried 5 times, not 5 times per retry.
			name:         "UnavailableForever",
			reply:        func(int) error { return status.Error(codes.Unavailable, "backend unavailable") },
			wantAttempts: 5,
			wantCode:     codes.Unavailable,
		},
		{
			name:         "Internal",
			reply:        func(int) error { return status.Error(codes.Internal, "internal") },
			wantAttempts: 5,
			wantCode:     codes.Internal,
		},
		{
			// DeadlineExceeded is NOT in retryableStatusCodes, so gRPC gives
			// up after one attempt. This is why the action cache lookup wraps
			// the call in its own DeadlineExceeded-only retry: without it a
			// timed-out probe is reported as a cache miss and the step runs
			// locally. See build.Cache.GetActionResult.
			name:         "DeadlineExceeded",
			reply:        func(int) error { return status.Error(codes.DeadlineExceeded, "deadline exceeded") },
			wantAttempts: 1,
			wantCode:     codes.DeadlineExceeded,
		},
		{
			// A non-retriable code fails fast, so a real cache miss stays one
			// round trip.
			name:         "NotFound",
			reply:        func(int) error { return status.Error(codes.NotFound, "not found") },
			wantAttempts: 1,
			wantCode:     codes.NotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &flakyActionCache{reply: tc.reply}
			cli := dialWithServiceConfig(t, cache)

			_, err := cli.GetActionResult(t.Context(), &rpb.GetActionResultRequest{
				InstanceName: "projects/test/instances/default_instance",
			})
			if got, want := status.Code(err), tc.wantCode; got != want {
				t.Errorf("GetActionResult code = %v, want %v (err: %v)", got, want, err)
			}
			if got, want := cache.count(), tc.wantAttempts; got != want {
				t.Errorf("server attempts = %d, want %d", got, want)
			}
		})
	}
}
