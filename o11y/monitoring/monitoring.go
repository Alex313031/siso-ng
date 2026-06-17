// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package monitoring provides OpenTelemetry support.
package monitoring

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	smetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/o11y/clog"
)

var (
	osFamilyKey       = "os_family"
	versionKey        = "siso_version"
	statusKey         = "status"
	remoteStatusKey   = "remote_status"
	isFallbackKey     = "is_fallback"
	exitCodeKey       = "exit_code"
	remoteExitCodeKey = "remote_exit_code"

	meter = otel.Meter("go.opentelemetry.io/otel/siso")

	// actionCount is a metric for tracking the number of actions.
	actionCount metric.Int64Counter
	// actionLatency is a metric for tracking the e2e latency of an action.
	actionLatency metric.Float64Histogram
	// buildCacheHitRatio is a metric of the ratio of cache hits in a build.
	buildCacheHitRatio metric.Float64Histogram
	// buildLatency is a metric for tracking the e2e latency of a build.
	buildLatency metric.Float64Histogram
	// buildCount is a metric for tracking the number of builds.
	buildCount metric.Int64Counter
	// reapiCancellations counts watchdog-triggered cancellations of
	// REAPI RPCs, tagged by call and reason.
	reapiCancellations metric.Int64Counter
	// reapiRetryDuration records latency and outcome of a retry that
	// followed a watchdog cancellation, so the cancel+retry payoff is
	// observable (call=which RPC, outcome=ok|err).
	reapiRetryDuration metric.Float64Histogram

	// mu protects updating staticMetricLabels.
	mu sync.Mutex
	// staticMetricLabels are the labels for all metrics.
	staticMetricLabels []attribute.KeyValue
)

func otelHandleError(ctx context.Context) otel.ErrorHandlerFunc {
	return func(err error) {
		clog.Warningf(ctx, "failed to export to OpenTelemetry: %v", err)
	}
}

// SetupViews sets up monitoring views. This can only be run once.
func SetupViews(ctx context.Context, version, rbeProject string, labels map[string]string) ([]smetric.View, error) {
	otel.SetErrorHandler(otelHandleError(ctx))

	if len(staticMetricLabels) != 0 {
		return nil, errors.New("views were already setup, cannot overwrite")
	}
	mu.Lock()
	defer mu.Unlock()

	staticMetricLabels = []attribute.KeyValue{
		attribute.String(osFamilyKey, runtime.GOOS),
		attribute.String(versionKey, version),
	}
	for k, v := range labels {
		staticMetricLabels = append(staticMetricLabels, attribute.String(k, v))
	}
	clog.Infof(ctx, "static labels for monitoring were set. %v", staticMetricLabels)

	var err error
	actionCount, err = meter.Int64Counter(
		"action.count",
		metric.WithDescription("Number of actions processed"),
		metric.WithUnit("{action}"),
	)
	if err != nil {
		return nil, err
	}

	actionLatency, err = meter.Float64Histogram(
		"action.latency",
		metric.WithDescription("Time spent processing an action"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, err
	}

	buildCacheHitRatio, err = meter.Float64Histogram(
		"build.cache_hit_ratio",
		metric.WithDescription("Ratio of cache hits in a build"),
		metric.WithUnit("{hit_ratio}"),
	)
	if err != nil {
		return nil, err
	}

	buildLatency, err = meter.Float64Histogram(
		"build.latency",
		metric.WithDescription("E2e build time spent in Siso"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	buildCount, err = meter.Int64Counter(
		"build.count",
		metric.WithDescription("Counter for builds"),
		metric.WithUnit("{unit}"),
	)
	if err != nil {
		return nil, err
	}

	reapiCancellations, err = meter.Int64Counter(
		"reapi.cancellations",
		metric.WithDescription("Watchdog-triggered cancellations of REAPI RPCs (call=which RPC, reason=which watchdog)."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, err
	}

	reapiRetryDuration, err = meter.Float64Histogram(
		"reapi.retry.duration",
		metric.WithDescription("Latency of a retry following a watchdog cancellation (call=which RPC, outcome=ok|err)."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	if err := setupBytestreamMetrics(); err != nil {
		return nil, err
	}

	views := []smetric.View{
		func(i smetric.Instrument) (smetric.Stream, bool) {
			s := smetric.Stream{Name: i.Name, Description: i.Description, Unit: i.Unit}
			switch i.Name {
			case "action.latency":
				s.Aggregation = smetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{1, 2, 3, 4, 5, 6, 8, 10, 13, 16, 20, 25, 30, 40, 50, 65, 80, 100, 130, 160, 200, 250, 300, 400, 500, 650, 800, 1000, 2000, 5000, 10000, 20000, 50000, 100000, 200000, 500000},
				}
			case "action.count":
				s.Aggregation = smetric.AggregationSum{}
			case "build.cache_hit_ratio":
				s.Aggregation = smetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{0.05, 0.1, 0.15, 0.20, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95, 1},
				}
			case "build.latency":
				s.Aggregation = smetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{1, 10, 60, 120, 300, 600, 1200, 2400, 3000, 3600, 4200, 4800, 5400, 6000, 6600, 7200, 9000, 10800, 12600, 14400},
				}
			case "build.count", "reapi.cancellations":
				s.Aggregation = smetric.AggregationSum{}
			case "reapi.retry.duration":
				s.Aggregation = smetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{0.05, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 30},
				}
			case "bytestream.read.ttfb",
				"bytestream.read.transport_pick",
				"bytestream.read.client_queue",
				"bytestream.read.server_setup",
				"bytestream.read.server_fetch",
				"bytestream.read.body_download":
				s.Aggregation = smetric.AggregationExplicitBucketHistogram{
					Boundaries: bytestreamHistogramBuckets,
				}
			default:
				return s, false
			}
			return s, true
		},
	}
	return views, nil
}

// NewMetricProvider returns a new Cloud monitoring metrics provider.
func NewMetricProvider(ctx context.Context, metricsProject, rbeProject string, exporter smetric.Exporter, views []smetric.View) (*smetric.MeterProvider, error) {
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithFromEnv(),
		resource.WithAttributes(
			semconv.ServiceNamespaceKey.String(rbeProject),
			// Manually set project: https://github.com/GoogleCloudPlatform/opentelemetry-operations-go/blob/94a7f44c3457d9e6583824b74eefc4610381238a/exporter/collector/config.go#L46C2-L46C72
			attribute.String("gcp.project.id", metricsProject)),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) && !errors.Is(err, resource.ErrSchemaURLConflict) {
		return nil, err
	}
	meterProvider := smetric.NewMeterProvider(
		smetric.WithResource(res),
		smetric.WithReader(smetric.NewPeriodicReader(exporter,
			smetric.WithInterval(1*time.Minute))),
		smetric.WithView(views...),
	)
	return meterProvider, nil
}

// RecordCancellation increments the reapi.cancellations counter.
// call: the RPC (e.g. "bytestream-read", "cache-check").
// reason: which watchdog fired (e.g. "pre_first_byte", "no_ops").
func RecordCancellation(ctx context.Context, call, reason string) {
	if reapiCancellations == nil {
		return
	}
	attrs := append([]attribute.KeyValue(nil), staticMetricLabels...)
	attrs = append(attrs,
		attribute.String("call", call),
		attribute.String("reason", reason),
	)
	reapiCancellations.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordRetryDuration records the latency and outcome of a retry that
// followed a watchdog cancellation. call: "cache-check", "bytestream-read".
func RecordRetryDuration(ctx context.Context, call string, d time.Duration, err error) {
	if reapiRetryDuration == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "err"
	}
	attrs := append([]attribute.KeyValue(nil), staticMetricLabels...)
	attrs = append(attrs,
		attribute.String("call", call),
		attribute.String("outcome", outcome),
	)
	reapiRetryDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))
}

// ExportActionMetrics exports metrics for one log record to OpenTelemetry.
func ExportActionMetrics(ctx context.Context, latency time.Duration, ar, remoteAr *rpb.ActionResult, actionErr, remoteErr error, cached, isFallback bool) {
	if !enabled() {
		return
	}
	// Use the same status values with CommandResultStatus in remote-apis-sdks to be aligned with Reclient. e.g. SUCCESS, CACHE_HIT
	// See also CommandResultStatus in remote-apis-sdks.
	// https://github.com/bazelbuild/remote-apis-sdks/blob/f4821a2a072c44f9af83002cf7a272fff8223fa3/go/api/command/command.proto#L172
	// TODO: Support REMOTE_ERROR, LOCAL_ERROR types if necessary.
	exitCode := ar.GetExitCode()
	var st string
	switch {
	case cached:
		st = "CACHE_HIT"
	case status.Code(actionErr) == codes.DeadlineExceeded || errors.Is(actionErr, context.DeadlineExceeded):
		st = "TIMEOUT"
	case exitCode != 0:
		st = "NON_ZERO_EXIT"
	default:
		st = "SUCCESS"
	}

	remoteExitCode := remoteAr.GetExitCode()
	var remoteStatus string
	switch {
	case cached:
		remoteStatus = "CACHE_HIT"
	case status.Code(remoteErr) == codes.DeadlineExceeded || errors.Is(remoteErr, context.DeadlineExceeded):
		remoteStatus = "TIMEOUT"
	case remoteExitCode != 0:
		remoteStatus = "NON_ZERO_EXIT"
	default:
		remoteStatus = "SUCCESS"
	}
	attributes := append(staticMetricLabels, []attribute.KeyValue{
		attribute.String(statusKey, st),
		attribute.Int64(exitCodeKey, int64(exitCode)),
		attribute.String(remoteStatusKey, remoteStatus),
		attribute.Bool(isFallbackKey, isFallback),
		attribute.Int64(remoteExitCodeKey, int64(remoteExitCode)),
	}...)
	actionCount.Add(ctx, 1, metric.WithAttributes(attributes...))
	actionLatency.Record(ctx, float64(latency)/1e6, metric.WithAttributes(attributes...))
}

// ExportBuildMetrics exports overall build metrics to OpenTelemetry.
func ExportBuildMetrics(ctx context.Context, latency time.Duration, cacheHitRatio float64, isErr bool) {
	if !enabled() {
		return
	}
	status := "SUCCESS"
	if isErr {
		status = "FAILURE"
	}
	attributes := append(staticMetricLabels, []attribute.KeyValue{
		attribute.String(statusKey, status),
	}...)
	buildCount.Add(ctx, 1, metric.WithAttributes(attributes...))
	buildLatency.Record(ctx, latency.Seconds(), metric.WithAttributes(attributes...))
	buildCacheHitRatio.Record(ctx, cacheHitRatio, metric.WithAttributes(attributes...))
}

func enabled() bool {
	return otel.GetMeterProvider() != nil &&
		actionCount != nil &&
		actionLatency != nil &&
		buildCount != nil &&
		buildLatency != nil &&
		buildCacheHitRatio != nil
}
