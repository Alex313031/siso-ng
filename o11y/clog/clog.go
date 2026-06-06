// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package clog provides context aware logging.
// It can store trace, spandID, arbitrary labels to each context.
// The main use case is to add build action context to each log entry automatically.
//
// TODO(b/269367111): It's also worth considering to use slog.
package clog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/golang/glog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otelog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/protobuf/proto"
)

// SwitchableGRPCLogger is a grpclog.LoggerV2 that can be updated with a new logger.
type SwitchableGRPCLogger struct {
	mu     sync.Mutex
	logger grpclog.LoggerV2
}

// NewSwitchableGRPCLogger creates a new SwitchableGRPCLogger.
func NewSwitchableGRPCLogger() *SwitchableGRPCLogger {
	return &SwitchableGRPCLogger{}
}

// SetLogger sets the logger.
func (s *SwitchableGRPCLogger) SetLogger(logger grpclog.LoggerV2) {
	s.mu.Lock()
	s.logger = logger
	s.mu.Unlock()
}

// Info logs to the INFO log.
func (s *SwitchableGRPCLogger) Info(args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Info(args...)
	}
}

// Infoln logs to the INFO log.
func (s *SwitchableGRPCLogger) Infoln(args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Infoln(args...)
	}
}

// Infof logs to the INFO log.
func (s *SwitchableGRPCLogger) Infof(format string, args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Infof(format, args...)
	}
}

// Warning logs to the WARNING log.
func (s *SwitchableGRPCLogger) Warning(args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Warning(args...)
	}
}

// Warningln logs to the WARNING log.
func (s *SwitchableGRPCLogger) Warningln(args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Warningln(args...)
	}
}

// Warningf logs to the WARNING log.
func (s *SwitchableGRPCLogger) Warningf(format string, args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Warningf(format, args...)
	}
}

// Error logs to the ERROR log.
func (s *SwitchableGRPCLogger) Error(args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Error(args...)
	}
}

// Errorln logs to the ERROR log.
func (s *SwitchableGRPCLogger) Errorln(args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Errorln(args...)
	}
}

// Errorf logs to the ERROR log.
func (s *SwitchableGRPCLogger) Errorf(format string, args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Errorf(format, args...)
	}
}

// Fatal logs to the FATAL log.
func (s *SwitchableGRPCLogger) Fatal(args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Fatal(args...)
	} else {
		glog.Fatal(args...)
	}
}

// Fatalln logs to the FATAL log.
func (s *SwitchableGRPCLogger) Fatalln(args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Fatalln(args...)
	} else {
		glog.Fatalln(args...)
	}
}

// Fatalf logs to the FATAL log.
func (s *SwitchableGRPCLogger) Fatalf(format string, args ...any) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger != nil {
		logger.Fatalf(format, args...)
	} else {
		glog.Fatalf(format, args...)
	}
}

// V reports whether verbosity level l is at least the requested verbose level.
func (s *SwitchableGRPCLogger) V(l int) bool {
	return bool(glog.VDepth(1, glog.Level(l)))
}

// https://cloud.google.com/logging/quotas
const logEntrySizeLimit = 256 * 1024

type contextKeyType int

var contextKey contextKeyType

// defaultFormatter doesn't set any context to the log content.
var defaultFormatter = func(e logging.Entry) string {
	return Message(e)
}

// New creates a new Logger.
func New(ctx context.Context, client *logging.Client, logID, accessLogID string, res *mrpb.MonitoredResource, collectorAddr string, opts ...logging.LoggerOption) (*Logger, error) {
	var otelLogger otelog.Logger
	var otelProvider *sdklog.LoggerProvider
	var otelGRPCConn *grpc.ClientConn
	directClient := client

	if collectorAddr != "" {
		conn, provider, logger, err := newOtelCollectorClient(ctx, collectorAddr, res)
		if err != nil {
			glog.Warningf("OTEL collector init failed, falling back to cloud logging: %v", err)
			if conn != nil {
				conn.Close()
			}
		} else {
			glog.Infof("OTEL logging is ready. Disabling direct cloud logging uploads.")
			directClient = nil
			otelGRPCConn = conn
			otelProvider = provider
			otelLogger = logger
		}
	}

	// recommends to use logging.CommonResource to set default resource for log entry.
	opts = append(opts, logging.CommonResource(res))
	var cl, acl *logging.Logger
	if directClient != nil {
		directClient.OnError = func(err error) {
			glog.Warningf("logger: %v", err)
		}

		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err := directClient.Ping(ctx)
		if err != nil {
			directClient.Close()
			return nil, fmt.Errorf("failed to ping logging service: %w", err)
		}
		cl = directClient.Logger(logID, opts...)
		acl = directClient.Logger(accessLogID, opts...)
	}

	logger := &Logger{
		Formatter:    defaultFormatter,
		client:       directClient,
		logger:       cl,
		accessLogger: acl,
		res:          res,
		otelLogger:   otelLogger,
		otelProvider: otelProvider,
		otelGRPCConn: otelGRPCConn,
	}
	if otelLogger != nil {
		glog.Infof("OTEL logging is ready: %s", logger.URL())
	} else if directClient != nil {
		glog.Infof("cloud logging is ready: %s", logger.URL())
	}

	logger.Infof("Binary: Built with %s %s for %s/%s", runtime.Compiler, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return logger, nil
}

// NewContext sets the given logger to the context.
func NewContext(ctx context.Context, logger *Logger) context.Context {
	return context.WithValue(ctx, contextKey, logger)
}

// NewSpan sets a new logger.Span with the given labels to the context.
func NewSpan(ctx context.Context, trace, spanID string, labels map[string]string) context.Context {
	logger, _ := ctx.Value(contextKey).(*Logger)
	return NewContext(ctx, logger.Span(trace, spanID, labels))
}

// FromContext returns a logger in the context, or returns a new logger.
func FromContext(ctx context.Context) *Logger {
	logger, ok := ctx.Value(contextKey).(*Logger)
	if !ok {
		return &Logger{
			Formatter: defaultFormatter,
		}
	}
	return logger
}

// Logger holds the trace, spanID, arbitrary labels of the context.
// It also can have custom formatter to generate a log content.
type Logger struct {
	// Formatter is a formatter of the entry for glog.
	// Default to `clog.Message(e)`.
	Formatter func(e logging.Entry) string

	// Additional log writer if non-nil
	writer io.Writer

	client *logging.Client
	// https://pkg.go.dev/cloud.google.com/go/logging#hdr-Grouping_Logs_by_Request
	// parent in access log has httprequest.request.{method,url} and httprequest.status
	// parent and child use the different log id.
	// parent and child use the same resource type and labels.
	// parent and child have the same trace field.
	logger       *logging.Logger
	accessLogger *logging.Logger

	res *mrpb.MonitoredResource

	// The following properties are equivalent to the ones in logging.LogEntry.
	// See the document for the details.
	// https://cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry
	trace  string
	spanID string
	labels map[string]string

	metricsLabels string

	otelLogger   otelog.Logger
	otelProvider *sdklog.LoggerProvider
	otelGRPCConn *grpc.ClientConn
}

func newOtelCollectorClient(ctx context.Context, collectorAddr string, res *mrpb.MonitoredResource) (*grpc.ClientConn, *sdklog.LoggerProvider, otelog.Logger, error) {
	conn, err := grpc.NewClient(collectorAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create gRPC connection to OTLP collector: %w", err)
	}
	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return conn, nil, nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}
	otelResource, err := sdkresource.New(ctx, sdkresource.WithAttributes(
		// Manually set project: https://github.com/GoogleCloudPlatform/opentelemetry-operations-go/blob/94a7f44c3457d9e6583824b74eefc4610381238a/exporter/collector/config.go#L46C2-L46C72
		attribute.String("gcp.project.id", res.Labels["project_id"]),
		semconv.ServiceName(res.Labels["job"]),
		semconv.ServiceInstanceID(res.Labels["task_id"]),
		semconv.ServiceNamespace(res.Labels["namespace"]),
		// https://github.com/GoogleCloudPlatform/opentelemetry-operations-go/blob/b50231bb7ac2630d764dea1fe6dc269121eab82f/internal/resourcemapping/resourcemapping.go#L147C21-L147C35
		semconv.CloudRegion(res.Labels["location"])))
	if err != nil {
		return conn, nil, nil, fmt.Errorf("failed to create OTLP resource: %w", err)
	}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(otelResource),
	)
	logger := provider.Logger("siso")
	return conn, provider, logger, nil
}

// SetMetricsLabels sets metricsLabels into logger (for errorReporting).
func (l *Logger) SetMetricsLabels(s string) {
	l.metricsLabels = s
}

// WithWriter returns logger with additional log writer.
func (l *Logger) WithWriter(w io.Writer) *Logger {
	if l == nil {
		l = &Logger{}
	}
	newLogger := &Logger{}
	*newLogger = *l
	newLogger.writer = w
	return newLogger
}

// URL returns url of cloud logging.
func (l *Logger) URL() string {
	if l == nil {
		return ""
	}
	// we use generic_task resource type, and it identifies the task
	// by task_id.
	// https://cloud.google.com/logging/docs/api/v2/resource-list
	return fmt.Sprintf("https://console.cloud.google.com/logs/viewer?project=%s&resource=%s/task_id/%s", l.res.Labels["project_id"], l.res.Type, l.res.Labels["task_id"])
}

// Span returns a sub logger for the trace span.
func (l *Logger) Span(trace, spanID string, labels map[string]string) *Logger {
	if l == nil {
		return &Logger{
			Formatter: defaultFormatter,
			trace:     trace,
			spanID:    spanID,
			labels:    labels,
		}
	}
	return &Logger{
		Formatter:    l.Formatter,
		client:       l.client,
		writer:       l.writer,
		logger:       l.logger,
		accessLogger: l.accessLogger,
		res:          l.res,
		trace:        trace,
		spanID:       spanID,
		labels:       labels,
		otelLogger:   l.otelLogger,
		otelProvider: l.otelProvider,
		otelGRPCConn: l.otelGRPCConn,
	}
}

// Log logs an entry.
func (l *Logger) Log(e logging.Entry) {
	l.log(e)
}

func toOtelValue(v any) otelog.Value {
	switch val := v.(type) {
	case string:
		return otelog.StringValue(val)
	case map[string]any:
		var kvs []otelog.KeyValue
		for k, v := range val {
			kvs = append(kvs, otelog.KeyValue{
				Key:   k,
				Value: toOtelValue(v),
			})
		}
		return otelog.MapValue(kvs...)

		// TODO: bool, float64, int64, []byte, slice
	default:
		return otelog.StringValue(fmt.Sprintf("%v", val))
	}
}

func (l *Logger) log(e logging.Entry) {
	if l != nil && l.otelLogger != nil {
		rec := otelog.Record{}
		rec.SetTimestamp(e.Timestamp)
		var s otelog.Severity
		switch e.Severity {
		case logging.Info:
			s = otelog.SeverityInfo
		case logging.Warning:
			s = otelog.SeverityWarn
		case logging.Error:
			s = otelog.SeverityError
		case logging.Critical:
			s = otelog.SeverityFatal
		case logging.Emergency:
			s = otelog.SeverityFatal
		default:
			s = otelog.SeverityInfo
		}
		rec.SetSeverity(s)
		rec.SetSeverityText(e.Severity.String())

		rec.SetBody(toOtelValue(e.Payload))

		ctx := context.Background()
		var otelTraceID trace.TraceID
		var otelSpanID trace.SpanID
		var err error

		if l.trace != "" {
			traceIDHex := l.trace
			if idx := strings.LastIndex(l.trace, "/"); idx != -1 {
				traceIDHex = l.trace[idx+1:]
			}
			otelTraceID, err = trace.TraceIDFromHex(traceIDHex)
			if err != nil {
				glog.Warningf("failed to parse trace ID %q: %v", traceIDHex, err)
			}
		}
		if l.spanID != "" {
			otelSpanID, err = trace.SpanIDFromHex(l.spanID)
			if err != nil {
				glog.Warningf("failed to parse span ID %q: %v", l.spanID, err)
			}
		}

		if otelTraceID.IsValid() && otelSpanID.IsValid() {
			sc := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: otelTraceID,
				SpanID:  otelSpanID,
			})
			ctx = trace.ContextWithSpanContext(ctx, sc)
		}

		var attrs []otelog.KeyValue
		for k, v := range e.Labels {
			attrs = append(attrs, otelog.String(k, v))
		}
		for k, v := range l.res.Labels {
			attrs = append(attrs, otelog.String(k, v))
		}
		if e.SourceLocation != nil {
			// Must use JSON instead of proto marshalling.
			// https://github.com/GoogleCloudPlatform/opentelemetry-operations-go/blob/b50231bb7ac2630d764dea1fe6dc269121eab82f/exporter/collector/logs.go#L718
			message, err := json.Marshal(e.SourceLocation)
			if err != nil {
				glog.Warning("failed to marshal source location: %v", err)
			} else {
				attrs = append(attrs, otelog.Bytes("gcp.source_location", message))
			}
		}

		// Magic name overriding key.
		// https://github.com/GoogleCloudPlatform/opentelemetry-operations-go/blob/b50231bb7ac2630d764dea1fe6dc269121eab82f/exporter/collector/logs.go#L60
		logName := "siso.log"
		if e.HTTPRequest != nil {
			logName = "siso.step"
		}
		attrs = append(attrs, otelog.String("gcp.log_name", logName))
		rec.AddAttributes(attrs...)
		l.otelLogger.Emit(ctx, rec)
		return
	}

	if l != nil && l.logger != nil {
		m, err := logging.ToLogEntry(e, "log-entry-project-name")
		if err != nil {
			glog.Warningf("toLogEntry: %v\n%v", err, m)
			return
		} else if s := proto.Size(m); s > logEntrySizeLimit {
			glog.Warningf("exceed size: %d\n%v", s, m)
			return
		}
	}
	if e.HTTPRequest != nil {
		if l == nil || l.accessLogger == nil {
			l.glogEntry(e)
			return
		}
		if e.Severity >= logging.Warning && e.HTTPRequest.Status >= 400 && e.HTTPRequest.Status < 499 {
			e.Severity = logging.Error
		}
		l.accessLogger.Log(e)
		if l.writer != nil {
			fmt.Fprintln(l.writer, l.Formatter(e))
		}
		return
	}
	if l == nil || l.logger == nil {
		l.glogEntry(e)
		return
	}
	l.logger.Log(e)
	if l.writer != nil {
		fmt.Fprintln(l.writer, l.Formatter(e))
	}
}

func (l *Logger) glogEntry(e logging.Entry) {
	msg := l.Formatter(e)
	switch e.Severity {
	case logging.Info:
		glog.InfoDepth(3, msg)
	case logging.Warning:
		glog.WarningDepth(3, msg)
	case logging.Error:
		glog.ErrorDepth(3, msg)
	case logging.Critical:
		glog.FatalDepth(3, msg)
	case logging.Emergency:
		glog.ExitDepth(3, msg)
	default:
		glog.InfoDepth(3, fmt.Sprintf("%s %s", e.Severity, msg))
	}
	if l.writer != nil {
		fmt.Fprintln(l.writer, msg)
	}
}

// Log logs an entry for the context.
func Log(ctx context.Context, e logging.Entry) {
	FromContext(ctx).log(e)
}

// LogSync logs an entry synchronously for the context.
func (l *Logger) LogSync(ctx context.Context, e logging.Entry) error {
	if e.HTTPRequest != nil {
		if l == nil || l.accessLogger == nil {
			l.glogEntry(e)
			return nil
		}
		return l.accessLogger.LogSync(ctx, e)
	}
	if l == nil || l.logger == nil {
		l.glogEntry(e)
		return nil
	}
	return l.logger.LogSync(ctx, e)
}

// LogSync logs an entry syncrhonously for the context.
func LogSync(ctx context.Context, e logging.Entry) error {
	return FromContext(ctx).LogSync(ctx, e)
}

// Info logs at info log level in the manner of fmt.Print.
func (l *Logger) Info(args ...any) {
	l.log(l.Entry(logging.Info, fmt.Sprint(args...)))
}

// Infoln logs at info log level in the manner of fmt.Println.
func (l *Logger) Infoln(args ...any) {
	l.log(l.Entry(logging.Info, fmt.Sprintln(args...)))
}

// Infof logs at info log level in the manner of fmt.Printf.
func (l *Logger) Infof(format string, args ...any) {
	l.log(l.Entry(logging.Info, fmt.Sprintf(format, args...)))
}

// Infof logs at info log level in the manner of fmt.Printf.
func Infof(ctx context.Context, format string, args ...any) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Info, fmt.Sprintf(format, args...)))
}

// Warning logs at warning log level in the manner of fmt.Print.
func (l *Logger) Warning(args ...any) {
	l.log(l.Entry(logging.Warning, fmt.Sprint(args...)))
}

// Warningln logs at warning log level in the manner of fmt.Println.
func (l *Logger) Warningln(args ...any) {
	l.log(l.Entry(logging.Warning, fmt.Sprintln(args...)))
}

// Warningf logs at warning log level in the manner of fmt.Printf.
func (l *Logger) Warningf(format string, args ...any) {
	l.log(l.Entry(logging.Warning, fmt.Sprintf(format, args...)))
}

// Warningf logs at warning log level in the manner of fmt.Printf.
func Warningf(ctx context.Context, format string, args ...any) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Warning, fmt.Sprintf(format, args...)))
}

// Error logs at error log level in the manner of fmt.Print.
func (l *Logger) Error(args ...any) {
	l.log(l.Entry(logging.Error, errorReportEntry(errors.New(fmt.Sprint(args...)), l.metricsLabels, debug.Stack())))
}

// Errorln logs at error log level in the manner of fmt.Println.
func (l *Logger) Errorln(args ...any) {
	l.log(l.Entry(logging.Error, errorReportEntry(errors.New(fmt.Sprintln(args...)), l.metricsLabels, debug.Stack())))
}

// Errorf logs at error log level in the manner of fmt.Printf.
func (l *Logger) Errorf(format string, args ...any) {
	l.log(l.Entry(logging.Error, errorReportEntry(fmt.Errorf(format, args...), l.metricsLabels, debug.Stack())))
}

// Errorf logs at error log level in the manner of fmt.Printf,
// and report error to errorreporting.
func Errorf(ctx context.Context, format string, args ...any) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Error, errorReportEntry(fmt.Errorf(format, args...), logger.metricsLabels, debug.Stack())))
}

// Fatal logs at fatal log level in the manner of fmt.Print with stacktrace, and exit.
func (l *Logger) Fatal(args ...any) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.fatalf(ctx, "%s", fmt.Sprint(args...))
}

// Fatalln logs at fatal log level in the manner of fmt.Println with stacktrace, and exit.
func (l *Logger) Fatalln(args ...any) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.fatalf(ctx, "%s", fmt.Sprintln(args...))
}

// Fatalf logs at fatal log level in the manner of fmt.Printf with stacktrace, and exit.
func (l *Logger) Fatalf(format string, args ...any) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.fatalf(ctx, format, args...)
}

func (l *Logger) fatalf(ctx context.Context, format string, args ...any) {
	err := l.LogSync(ctx, l.Entry(logging.Critical, errorReportEntry(fmt.Errorf(format, args...), l.metricsLabels, debug.Stack())))
	if err != nil {
		glog.ErrorDepth(1, fmt.Sprintf("logSync: %v", err))
	}
	glog.FatalDepth(2, fmt.Sprintf(format, args...))
}

// Fatalf logs at fatal log level in the manner of fmt.Printf with stacktrace, and exit.
func Fatalf(ctx context.Context, format string, args ...any) {
	logger := FromContext(ctx)
	logger.fatalf(ctx, format, args...)
}

// Exitf logs at fatal log level in the manner of fmt.Printf, and exit.
func (l *Logger) Exitf(format string, args ...any) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.exitf(ctx, format, args...)
}

func (l *Logger) exitf(ctx context.Context, format string, args ...any) {
	err := l.LogSync(ctx, l.Entry(logging.Emergency, errorReportEntry(fmt.Errorf(format, args...), l.metricsLabels, debug.Stack())))
	if err != nil {
		glog.ErrorDepth(1, fmt.Sprintf("logSync: %v", err))
	}
	glog.ExitDepth(2, fmt.Sprintf(format, args...))
}

// Exitf logs at fatal log level in the manner of fmt.Printf, and exit.
func Exitf(ctx context.Context, format string, args ...any) {
	logger := FromContext(ctx)
	logger.exitf(ctx, format, args...)
}

// https://docs.cloud.google.com/error-reporting/docs/formatting-error-messages
// https://github.com/googleapis/google-cloud-go/blob/errorreporting/v0.4.0/errorreporting/errors.go#L180
func errorReportEntry(err error, metricsLabels string, stack []byte) map[string]any {
	return map[string]any{
		"@type":       "type.googleapis.com/google.devtools.clouderrorreporting.v1beta1.ReportedErrorEvent",
		"message":     err.Error(),
		"stack_trace": err.Error() + " " + metricsLabels + "\n" + string(stack),
	}
}

// Message returns message in logging.
func Message(e logging.Entry) string {
	if m, ok := e.Payload.(map[string]any); ok {
		return fmt.Sprintf("%v", m["message"])
	}
	return fmt.Sprintf("%v", e.Payload)
}

// Entry creates a new log entry for the given severity.
func (l *Logger) Entry(severity logging.Severity, payload any) logging.Entry {
	var loc *loggingpb.LogEntrySourceLocation
	pc := make([]uintptr, 10)
	n := runtime.Callers(1, pc)
	if n > 0 {
		pc = pc[:n]
		frames := runtime.CallersFrames(pc)
		for {
			frame, more := frames.Next()
			switch {
			case strings.HasSuffix(frame.File, "clog/clog.go"):
			case filepath.Base(filepath.Dir(frame.File)) == "grpclog":
			default:
				loc = &loggingpb.LogEntrySourceLocation{
					File:     filepath.Base(frame.File),
					Line:     int64(frame.Line),
					Function: frame.Function,
				}
			}
			if !more || loc != nil {
				break
			}
		}
	}
	return logging.Entry{
		Timestamp:      time.Now(),
		Severity:       severity,
		Payload:        payload,
		Labels:         l.labels,
		SourceLocation: loc,
		Trace:          l.trace,
		SpanID:         l.spanID,
	}
}

// V checks at verbose log level.
func (l *Logger) V(level int) bool {
	return bool(glog.VDepth(1, glog.Level(level)))
}

// Close closes the logger. it will flush log entries.
func (l *Logger) Close() error {
	l.Infof("close log")
	glog.Flush()
	if l == nil {
		return nil
	}
	var lerr, aerr error
	if l.logger != nil {
		lerr = l.logger.Flush()
	}
	if l.accessLogger != nil {
		aerr = l.accessLogger.Flush()
	}
	if l.otelProvider != nil {
		if err := l.otelProvider.Shutdown(context.Background()); err != nil {
			glog.Warningf("failed to shutdown OTLP log provider: %v", err)
		}
	}
	if l.otelGRPCConn != nil {
		if err := l.otelGRPCConn.Close(); err != nil {
			glog.Warningf("failed to close OTLP gRPC connection: %v", err)
		}
	}
	// not close client to avoid 'panic: send on closed channel'
	// b/282860686
	// https://github.com/googleapis/google-cloud-go/issues/7944
	if lerr != nil || aerr != nil {
		return fmt.Errorf("failed to flush logging: %w, %w", lerr, aerr)
	}
	return nil
}
