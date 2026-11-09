// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"net"
	"testing"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TestTracingDialOptionPropagatesGrpcTraceBin verifies that the dial option
// installed when Option.TracerProvider is set propagates the parent span
// context to the server in the grpc-trace-bin header, sampled. That is what
// makes an action's RBE RPCs join the step's trace server-side (e.g. Dapper).
func TestTracingDialOptionPropagatesGrpcTraceBin(t *testing.T) {
	// In-process server that captures the incoming metadata of any call.
	gotMD := make(chan metadata.MD, 1)
	handler := func(_ any, stream grpc.ServerStream) error {
		md, _ := metadata.FromIncomingContext(stream.Context())
		gotMD <- md
		return nil
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(handler))
	go srv.Serve(lis)
	defer srv.Stop()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer func() { _ = tp.Shutdown(t.Context()) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		otelDialOption(tp, metricnoop.NewMeterProvider()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer conn.Close()

	// Inject a parent span context with a known trace id, as runStep does from
	// the step's RawIDs.
	wantTrace := oteltrace.TraceID{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01}
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    wantTrace,
		SpanID:     oteltrace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: oteltrace.FlagsSampled,
	})
	ctx := oteltrace.ContextWithSpanContext(t.Context(), sc)

	_ = conn.Invoke(ctx, "/probe.Service/Probe", &emptypb.Empty{}, &emptypb.Empty{})

	md := <-gotMD
	bin := md.Get("grpc-trace-bin")
	if len(bin) != 1 {
		t.Fatalf("grpc-trace-bin entries = %d, want 1", len(bin))
	}
	// Binary format: b[0]=version, b[1]=0, b[2:18]=traceID, b[18]=1,
	// b[19:27]=spanID, b[27]=2, b[28]=flags.
	b := []byte(bin[0])
	if len(b) != 29 {
		t.Fatalf("grpc-trace-bin len = %d, want 29", len(b))
	}
	var gotTrace oteltrace.TraceID
	copy(gotTrace[:], b[2:18])
	if gotTrace != wantTrace {
		t.Errorf("propagated trace id = %s, want %s", gotTrace, wantTrace)
	}
	if !oteltrace.TraceFlags(b[28]).IsSampled() {
		t.Errorf("grpc-trace-bin not sampled (flags=%02x)", b[28])
	}
}

// TestCookieInterceptorAttachesCookie verifies the cookie interceptor adds the
// cookie header to RPCs on the connection (and, by being a per-connection
// interceptor, only there).
func TestCookieInterceptorAttachesCookie(t *testing.T) {
	const cookie = "TR=T=test:X=x:S=sig"
	gotMD := make(chan metadata.MD, 1)
	handler := func(_ any, stream grpc.ServerStream) error {
		md, _ := metadata.FromIncomingContext(stream.Context())
		gotMD <- md
		return nil
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(handler))
	go srv.Serve(lis)
	defer srv.Stop()

	unary, stream := cookieInterceptors(cookie)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(unary),
		grpc.WithChainStreamInterceptor(stream),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer conn.Close()

	_ = conn.Invoke(t.Context(), "/probe.Service/Probe", &emptypb.Empty{}, &emptypb.Empty{})

	md := <-gotMD
	if got := md.Get("cookie"); len(got) != 1 || got[0] != cookie {
		t.Errorf("cookie metadata = %q, want exactly [%q]", got, cookie)
	}
}
