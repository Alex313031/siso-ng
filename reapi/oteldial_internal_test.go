// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"net"
	"slices"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	oteltrace "go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/auth/cred"
)

func collectMetricNames(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}
	return names
}

func containsMetric(names []string, want string) bool {
	return slices.Contains(names, want)
}

// TestOTelDialOptionRecordsMetrics checks that the OpenTelemetry dial option
// records grpc client metrics, not just traces. Dial disables gtransport's
// default telemetry handler when tracing is on, so if this option does not
// record metrics, traced builds record none at all.
func TestOTelDialOptionRecordsMetrics(t *testing.T) {
	ctx := t.Context()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(t.Context()) }()
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		otelDialOption(tracenoop.NewTracerProvider(), mp))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The server has no registered services, so the RPC fails with
	// Unimplemented. The call still records client metrics.
	err = conn.Invoke(ctx, "/build.bazel.remote.execution.v2.ActionCache/GetActionResult",
		&rpb.GetActionResultRequest{}, &rpb.ActionResult{})
	if got, want := status.Code(err), codes.Unimplemented; got != want {
		t.Fatalf("Invoke status = %v, want %v: %v", got, want, err)
	}

	names := collectMetricNames(t, reader)
	slices.Sort(names)
	want := []string{
		"grpc.client.attempt.duration",
		"grpc.client.attempt.rcvd_total_compressed_message_size",
		"grpc.client.attempt.sent_total_compressed_message_size",
		"grpc.client.attempt.started",
		"grpc.client.call.duration",
	}
	if !slices.Equal(names, want) {
		t.Errorf("recorded metrics = %q, want %q", names, want)
	}
}

func TestNewConnRecordsNativeMetrics(t *testing.T) {
	ctx := t.Context()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	for _, tc := range []struct {
		name           string
		tracerProvider oteltrace.TracerProvider
	}{
		{name: "without_tracing"},
		{name: "with_tracing", tracerProvider: tracenoop.NewTracerProvider()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(t.Context()) }()
			conn, err := newConn(ctx, lis.Addr().String(), cred.Cred{}, 0, Option{
				Insecure:       true,
				TracerProvider: tc.tracerProvider,
				MeterProvider:  mp,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			_, err = rpb.NewActionCacheClient(conn).GetActionResult(ctx, &rpb.GetActionResultRequest{})
			if got, want := status.Code(err), codes.Unimplemented; got != want {
				t.Fatalf("GetActionResult status = %v, want %v: %v", got, want, err)
			}

			names := collectMetricNames(t, reader)
			if !containsMetric(names, "grpc.client.call.duration") {
				t.Errorf("recorded metrics = %q, want grpc.client.call.duration among them", names)
			}
			if containsMetric(names, "rpc.client.call.duration") {
				t.Errorf("recorded metrics = %q, do not want rpc.client.call.duration", names)
			}
		})
	}
}
