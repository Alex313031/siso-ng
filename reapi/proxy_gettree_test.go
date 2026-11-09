// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"
)

// fakeCAS streams pages for GetTree and then ends the stream.
type fakeCAS struct {
	rpb.UnimplementedContentAddressableStorageServer
	pages []*rpb.GetTreeResponse
}

func (f *fakeCAS) GetTree(_ *rpb.GetTreeRequest, serv grpc.ServerStreamingServer[rpb.GetTreeResponse]) error {
	for _, p := range f.pages {
		if err := serv.Send(p); err != nil {
			return err
		}
	}
	return nil
}

// dialProxiedCAS serves backend on one bufconn, puts a CAS proxy in front of it
// on another, and returns a client for the proxy.
func dialProxiedCAS(t *testing.T, backend rpb.ContentAddressableStorageServer) rpb.ContentAddressableStorageClient {
	t.Helper()

	dial := func(lis *bufconn.Listener) *grpc.ClientConn {
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	serve := func(reg func(*grpc.Server)) *bufconn.Listener {
		lis := bufconn.Listen(1 << 20)
		srv := grpc.NewServer()
		reg(srv)
		go srv.Serve(lis)
		t.Cleanup(srv.Stop)
		return lis
	}

	backendLis := serve(func(s *grpc.Server) { rpb.RegisterContentAddressableStorageServer(s, backend) })
	cp := &contentAddressableStorageProxy{
		client:            rpb.NewContentAddressableStorageClient(dial(backendLis)),
		writableInstances: make(map[string]bool),
	}
	proxyLis := serve(func(s *grpc.Server) { rpb.RegisterContentAddressableStorageServer(s, cp) })
	return rpb.NewContentAddressableStorageClient(dial(proxyLis))
}

// getTreePages reads a GetTree stream to completion.
func getTreePages(ctx context.Context, cli rpb.ContentAddressableStorageClient) ([]*rpb.GetTreeResponse, error) {
	stream, err := cli.GetTree(ctx, &rpb.GetTreeRequest{})
	if err != nil {
		return nil, err
	}
	var got []*rpb.GetTreeResponse
	for {
		resp, err := stream.Recv()
		if err == io.EOF { //nolint:errorlint
			return got, nil
		}
		if err != nil {
			return got, err
		}
		got = append(got, resp)
	}
}

// TestGetTreeProxy checks that the proxy ends the client's stream cleanly for
// the shapes a backend may produce, including a last page that still carries a
// next_page_token and a tree with no pages at all.
func TestGetTreeProxy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []*rpb.GetTreeResponse
	}{
		{
			name:  "EmptyTree",
			pages: nil,
		},
		{
			name:  "LastPageWithoutToken",
			pages: []*rpb.GetTreeResponse{{NextPageToken: ""}},
		},
		{
			// The backend may end the stream while still reporting a token, which
			// tells the client to ask for the next page in a new GetTree call.
			name:  "LastPageWithToken",
			pages: []*rpb.GetTreeResponse{{NextPageToken: "page-2"}},
		},
		{
			name:  "MultiplePages",
			pages: []*rpb.GetTreeResponse{{NextPageToken: "page-2"}, {NextPageToken: ""}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := dialProxiedCAS(t, &fakeCAS{pages: tc.pages})
			got, err := getTreePages(t.Context(), cli)
			if err != nil {
				t.Errorf("GetTree: %v; want clean end of stream", err)
			}
			if len(got) != len(tc.pages) {
				t.Errorf("pages = %d, want %d", len(got), len(tc.pages))
			}
			for i := range got {
				if i >= len(tc.pages) {
					break
				}
				if got, want := got[i].GetNextPageToken(), tc.pages[i].GetNextPageToken(); got != want {
					t.Errorf("page %d next_page_token = %q, want %q", i, got, want)
				}
			}
		})
	}
}
