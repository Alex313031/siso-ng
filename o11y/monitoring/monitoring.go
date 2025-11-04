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

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	smetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/o11y/clog"
)

var (
	osFamilyKey       = "os_family"
	versionKey        = "siso_version"
	statusKey         = "status"
	remoteStatusKey   = "remote_status"
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
			case "build.count":
				s.Aggregation = smetric.AggregationSum{}
			default:
				return s, false
			}
			return s, true
		},
	}
	return views, nil
}

// NewMetricProvider returns a new Cloud monitoring metrics provider.
func NewMetricProvider(ctx context.Context, rbeProject string, exporter smetric.Exporter, views []smetric.View) (*smetric.MeterProvider, error) {
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithFromEnv(),
		resource.WithAttributes(semconv.ServiceNamespaceKey.String(rbeProject)),
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

// ExportActionMetrics exports metrics for one log record to OpenTelemetry.
func ExportActionMetrics(ctx context.Context, latency time.Duration, ar, remoteAr *rpb.ActionResult, actionErr, remoteErr error, cached bool) {
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
