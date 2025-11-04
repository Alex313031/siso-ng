// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package trace

import (
	"context"
	"path"
	"sync"
	"time"

	trace "cloud.google.com/go/trace/apiv2"
	"cloud.google.com/go/trace/apiv2/tracepb"
	log "github.com/golang/glog"
	"google.golang.org/api/option"

	"go.chromium.org/build/siso/o11y/clog"
)

// Options is options for trace exporter.
type Options struct {
	ProjectID string

	ServiceName string

	// threshold of step duration to export.
	// it will not export step trace if step duration is less than this.
	StepThreshold time.Duration

	// threshold of span duration to export.
	// it will not export span trace if span duration is less than this.
	SpanThreshold time.Duration
	ClientOptions []option.ClientOption
}

// Exporter is trace exporter.
type Exporter struct {
	ProjectID     string
	ServiceName   string
	stepThreshold time.Duration
	spanThreshold time.Duration
	client        *trace.Client

	mu      sync.Mutex
	closed  bool
	batches []*tracepb.Span
	wg      sync.WaitGroup
	q       chan []*tracepb.Span
}

// NewExporter creates new trace exporter.
func NewExporter(ctx context.Context, opts Options) (*Exporter, error) {
	e := &Exporter{
		ProjectID:     opts.ProjectID,
		ServiceName:   opts.ServiceName,
		stepThreshold: opts.StepThreshold,
		spanThreshold: opts.SpanThreshold,
		q:             make(chan []*tracepb.Span, 1000),
	}
	var err error
	e.client, err = trace.NewClient(ctx, opts.ClientOptions...)
	if err != nil {
		return nil, err
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			select {
			case batch, ok := <-e.q:
				if !ok {
					clog.Infof(ctx, "trace exporter terminated")
					return
				}
				if len(batch) == 0 {
					continue
				}
				err := e.client.BatchWriteSpans(ctx, &tracepb.BatchWriteSpansRequest{
					Name:  path.Join("projects", e.ProjectID),
					Spans: batch,
				})
				if err != nil {
					clog.Warningf(ctx, "failed to export spans: %v\n%v", err, batch)
				}
			case <-ctx.Done():
				clog.Infof(ctx, "trace exporter finishes: %v", context.Cause(ctx))
				return
			}
		}
	}()
	return e, nil
}

// Close flushes all pending traces and closes the exporter.
func (e *Exporter) Close(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		clog.Infof(ctx, "exporter close. last batch=%d q=%d", len(e.batches), len(e.q))
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
		e.q <- e.batches
		close(e.q)
		t := time.Now()
		e.wg.Wait()
		clog.Infof(ctx, "exporter finish: %s", time.Since(t))
		e.client.Close()
		close(done)
	}()
	select {
	case <-time.After(1 * time.Second):
		clog.Warningf(ctx, "close not finished in 1sec")
	case <-done:
		clog.Infof(ctx, "exporter closed.")
	}
}

// Export exports a trace.
func (e *Exporter) Export(ctx context.Context, tc *Context) {
	if e == nil || tc == nil {
		return
	}
	if len(tc.spans) == 0 {
		return
	}
	var spans []*tracepb.Span
	var ndropped int
	for _, s := range tc.spans {
		span := s.proto(ctx, e.ProjectID)
		if span == nil {
			continue
		}
		end := span.EndTime.AsTime()
		start := span.StartTime.AsTime()
		if end.Sub(start) < e.spanThreshold {
			if log.V(1) {
				clog.Infof(ctx, "drop short span %s: %v", span.DisplayName, end.Sub(start))
			}
			ndropped++
			continue
		}
		if e.ServiceName != "" {
			if span.Attributes == nil {
				span.Attributes = &tracepb.Span_Attributes{}
			}
			attrs := span.Attributes
			if attrs.AttributeMap == nil {
				attrs.AttributeMap = make(map[string]*tracepb.AttributeValue)
			}
			if _, ok := attrs.AttributeMap["service.name"]; !ok {
				attrs.AttributeMap["service.name"] = attrValue(e.ServiceName)
			}
		}
		spans = append(spans, span)
	}
	if log.V(1) {
		clog.Infof(ctx, "export %d -> %d traces in %s", len(tc.spans), len(spans), e.ProjectID)
	}
	if len(spans) == 0 {
		return
	}
	// spans[0] is step span.
	end := spans[0].EndTime.AsTime()
	start := spans[0].StartTime.AsTime()
	if end.Sub(start) < e.stepThreshold {
		clog.Infof(ctx, "ignore %d (dropped:%d) traces %s: %s", len(spans), ndropped, spans[0].Name, end.Sub(start))
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.batches = append(e.batches, spans...)
	var batches []*tracepb.Span
	if len(e.batches) > 1500 {
		batches = e.batches
		e.batches = nil
	}
	if len(batches) == 0 {
		return
	}
	if e.closed {
		return
	}
	select {
	case e.q <- batches:
	case <-ctx.Done():
	}
}
