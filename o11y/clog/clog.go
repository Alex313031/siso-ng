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
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/golang/glog"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
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
	return fmt.Sprintf("%v", e.Payload)
}

// New creates a new Logger.
func New(ctx context.Context, client *logging.Client, logID, accessLogID string, res *mrpb.MonitoredResource, opts ...logging.LoggerOption) (*Logger, error) {
	client.OnError = func(err error) {
		glog.Warningf("logger: %v", err)
	}
	err := client.Ping(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to ping logging service: %w", err)
	}
	// recommends to use logging.CommonResource to set default resource for log entry.
	opts = append(opts, logging.CommonResource(res))
	logger := &Logger{
		Formatter:    defaultFormatter,
		client:       client,
		logger:       client.Logger(logID, opts...),
		accessLogger: client.Logger(accessLogID, opts...),
		res:          res,
	}
	glog.Infof("cloud logging is ready: %s", logger.URL())

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
	// Default to `fmt.Sprintf("%v", e.Payload)`.
	Formatter func(e logging.Entry) string

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
		logger:       l.logger,
		accessLogger: l.accessLogger,
		res:          l.res,
		trace:        trace,
		spanID:       spanID,
		labels:       labels,
	}
}

// Log logs an entry.
func (l *Logger) Log(e logging.Entry) {
	l.log(e)
}

func (l *Logger) log(e logging.Entry) {
	if l != nil && l.logger != nil {
		m, err := logging.ToLogEntry(e, "log-entry-project-name")
		if err != nil {
			glog.Warningf("toLogEntry: %v\n%v", err, m)
		} else if s := proto.Size(m); s > logEntrySizeLimit {
			glog.Warningf("exceed size: %d\n%v", s, m)
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
		return
	}
	if l == nil || l.logger == nil {
		l.glogEntry(e)
		return
	}
	l.logger.Log(e)
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
	l.log(l.Entry(logging.Error, fmt.Sprint(args...)))
}

// Errorln logs at error log level in the manner of fmt.Println.
func (l *Logger) Errorln(args ...any) {
	l.log(l.Entry(logging.Error, fmt.Sprintln(args...)))
}

// Errorf logs at error log level in the manner of fmt.Printf.
func (l *Logger) Errorf(format string, args ...any) {
	l.log(l.Entry(logging.Error, fmt.Sprintf(format, args...)))
}

// Errorf logs at error log level in the manner of fmt.Printf.
func Errorf(ctx context.Context, format string, args ...any) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Error, fmt.Sprintf(format, args...)))
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
	err := l.LogSync(ctx, l.Entry(logging.Critical, fmt.Sprintf(format, args...)))
	if err != nil {
		glog.ErrorDepth(1, fmt.Sprintf("logSync: %v", err))
	}
	glog.FatalDepth(2, fmt.Sprintf(format, args...))
}

// Fatalf logs at fatal log level in the manner of fmt.Printf with stacktrace, and exit.
func Fatalf(ctx context.Context, format string, args ...any) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Critical, fmt.Sprintf(format, args...)))
}

// Exitf logs at fatal log level in the manner of fmt.Printf, and exit.
func (l *Logger) Exitf(format string, args ...any) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.exitf(ctx, format, args...)
}

func (l *Logger) exitf(ctx context.Context, format string, args ...any) {
	err := l.LogSync(ctx, l.Entry(logging.Emergency, fmt.Sprintf(format, args...)))
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
	if l.client == nil {
		return nil
	}
	var lerr, aerr error
	if l.logger != nil {
		lerr = l.logger.Flush()
	}
	if l.accessLogger != nil {
		aerr = l.accessLogger.Flush()
	}
	// not close client to avoid 'panic: send on closed channel'
	// b/282860686
	// https://github.com/googleapis/google-cloud-go/issues/7944
	if lerr != nil || aerr != nil {
		return fmt.Errorf("failed to flush logging: %w, %w", lerr, aerr)
	}
	return nil
}
