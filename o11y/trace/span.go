// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package trace manages execution traces.
package trace

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/trace/apiv2/tracepb"
	log "github.com/golang/glog"
	"github.com/google/uuid"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.chromium.org/build/siso/o11y/clog"
)

// Context is a trace context.
type Context struct {
	// https://cloud.google.com/trace/docs/reference/v2/rest/v2/projects.traces/batchWrite#Span
	// [TRACE_ID] is a unique identifier for a trace within a project;
	// it is a 32-character hexadecimal encoding of a 16-byte array.
	// It should not be zero.
	traceID [16]byte

	mu sync.Mutex
	// first span is the top span in the trace.
	spans []*Span
}

// New creates a new context for id (uuid).
func New(ctx context.Context, id string) *Context {
	if log.V(2) {
		clog.Infof(ctx, "new trace context for %s", id)
	}
	u, err := uuid.Parse(id)
	if err != nil {
		clog.Errorf(ctx, "bad id %q: %v", id, err)
	}
	return &Context{
		traceID: u,
	}
}

// NewSpan creates new span in the parent.
func (t *Context) NewSpan(ctx context.Context, name string, parent *Span) *Span {
	if t == nil {
		return nil
	}
	return t.newSpan(ctx, name, parent)
}

// Spans returns span data in the trace context.
func (t *Context) Spans() []SpanData {
	var data []SpanData
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.spans {
		sd := s.data()
		if sd.Name == "" {
			continue
		}
		data = append(data, sd)
	}
	return data
}

// SpanProtos returns span protos in the trace context.
func (t *Context) SpanProtos(ctx context.Context, projectID string) []*tracepb.Span {
	var spans []*tracepb.Span
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.spans {
		span := s.proto(ctx, projectID)
		if span == nil {
			continue
		}
		spans = append(spans, span)
	}
	return spans
}

var invalidSpanID [8]byte

func (t *Context) newSpan(ctx context.Context, name string, parent *Span) *Span {
	span := &Span{
		t:           t,
		parent:      parent,
		displayName: name,
		start:       time.Now(),
	}
	// https://github.com/open-telemetry/opentelemetry-go/blob/main/sdk%2Ftrace%2Fid_generator.go
	for {
		binary.NativeEndian.PutUint64(span.spanID[:], rand.Uint64())
		if span.spanID != invalidSpanID {
			break
		}
	}

	span.attrs = span.attrBuf[:0] // assign a new empty slice with the pre-allocated capacity (=2) for attrBuf.

	// This function is called in hot path, so we need to minimize the lock time.
	t.mu.Lock()
	if parent == nil && len(t.spans) > 0 {
		span.parent = t.spans[0]
	}
	t.spans = append(t.spans, span)
	t.mu.Unlock()

	if log.V(2) {
		clog.Infof(ctx, "new span %s %q<%v", name, span.spanID, span.parent)
	}
	return span
}

type contextKeyType int

const (
	contextKey contextKeyType = iota
	spanKey
)

// NewContext returns new context with a trace context.
func NewContext(ctx context.Context, t *Context) context.Context {
	return context.WithValue(ctx, contextKey, t)
}

// NewSpan returns new contexts and span.
// If no trace context, returns nil span.
func NewSpan(ctx context.Context, name string) (context.Context, *Span) {
	t, ok := ctx.Value(contextKey).(*Context)
	if !ok || t == nil {
		return ctx, nil
	}
	parent, _ := ctx.Value(spanKey).(*Span)
	span := t.NewSpan(ctx, name, parent)
	return context.WithValue(ctx, spanKey, span), span
}

// ID returns the trace id.
func ID(ctx context.Context) string {
	t, ok := ctx.Value(contextKey).(*Context)
	if !ok {
		return ""
	}
	return uuid.UUID(t.traceID).String()
}

// CurSpan returns current span in the context.
func CurSpan(ctx context.Context) *Span {
	span, ok := ctx.Value(spanKey).(*Span)
	if !ok {
		return nil
	}
	return span
}

// Span is a trace span.
type Span struct {
	t      *Context
	spanID [8]byte
	parent *Span

	mu          sync.Mutex
	displayName string
	start       time.Time
	end         time.Time
	attrs       []attrKV
	attrBuf     [2]attrKV // avoid allocation for small number of attrs.
	status      *spb.Status
}

type attrKV struct {
	key   string
	value any
}

// SetAttr sets attributes in the span.
func (s *Span) SetAttr(key string, value any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs = append(s.attrs, attrKV{key: key, value: value})
}

// Add adds span data as a child of the span and returns it.
func (s *Span) Add(ctx context.Context, sd SpanData) *Span {
	if s == nil {
		return nil
	}
	if s.t == nil {
		return nil
	}
	s.mu.Lock()
	ss := s.t.newSpan(ctx, sd.Name, s)
	s.mu.Unlock()
	ss.start = sd.Start
	ss.end = sd.End
	var keys []string
	for key := range sd.Attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ss.attrs = append(ss.attrs, attrKV{key: key, value: sd.Attrs[key]})
	}
	ss.status = sd.Status
	return ss
}

// Close closes the span.
func (s *Span) Close(st *spb.Status) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.end = time.Now()
	s.status = st
}

func (s *Span) protoAttrs() *tracepb.Span_Attributes {
	if s == nil {
		return nil
	}
	m := make(map[string]*tracepb.AttributeValue)
	for _, kv := range s.attrs {
		av := attrValue(kv.value)
		if av != nil {
			m[kv.key] = av
		}
	}
	return &tracepb.Span_Attributes{
		AttributeMap: m,
	}
}

func (s *Span) data() SpanData {
	if s == nil {
		return SpanData{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	end := s.end
	if end.IsZero() {
		end = time.Now()
	}
	attrs := make(map[string]any)
	for _, a := range s.attrs {
		attrs[a.key] = a.value
	}
	return SpanData{
		Name:   s.displayName,
		Start:  s.start,
		End:    end,
		Attrs:  attrs,
		Status: s.status,
	}
}

// ID returns trace and span id.
func (s *Span) ID(projectID string) (trace, span string) {
	return path.Join("projects", projectID, "traces", hex.EncodeToString(s.t.traceID[:])), hex.EncodeToString(s.spanID[:])
}

func (s *Span) proto(ctx context.Context, projectID string) *tracepb.Span {
	if s == nil {
		return nil
	}
	if projectID == "" {
		return nil
	}
	spanID := hex.EncodeToString(s.spanID[:])
	parent := ""
	if s.parent != nil {
		parent = hex.EncodeToString(s.parent.spanID[:])
		if spanID == parent {
			clog.Fatalf(ctx, "spanID == parent? %q span=%p[%q] parent=%p[%q]", spanID, s, s.displayName, s.parent, s.parent.displayName)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &tracepb.Span{
		Name: path.Join("projects", projectID,
			"traces", hex.EncodeToString(s.t.traceID[:]),
			"spans", spanID),
		SpanId:       spanID,
		ParentSpanId: parent,
		DisplayName:  truncatableString(s.displayName, 128),
		StartTime:    timestamppb.New(s.start),
		EndTime:      timestamppb.New(s.end),
		Attributes:   s.protoAttrs(),
		Status:       s.status,
		SpanKind:     tracepb.Span_INTERNAL,
	}
}

func truncatableString(s string, limit int) *tracepb.TruncatableString {
	n := len(s)
	if n < limit {
		return &tracepb.TruncatableString{Value: s}
	}
	return &tracepb.TruncatableString{
		Value:              s[:limit],
		TruncatedByteCount: int32(n - limit),
	}
}

func attrValue(v any) *tracepb.AttributeValue {
	var iv int64
	switch v := v.(type) {
	case []string:
		return nil
	case string:
		return &tracepb.AttributeValue{
			Value: &tracepb.AttributeValue_StringValue{StringValue: truncatableString(v, 256)},
		}
	case bool:
		return &tracepb.AttributeValue{
			Value: &tracepb.AttributeValue_BoolValue{BoolValue: v},
		}
	case int:
		iv = int64(v)
	case uint:
		iv = int64(v)
	case int8:
		iv = int64(v)
	case uint8:
		iv = int64(v)
	case int16:
		iv = int64(v)
	case uint16:
		iv = int64(v)
	case int32:
		iv = int64(v)
	case uint32:
		iv = int64(v)
	case int64:
		iv = v
	case uint64:
		iv = int64(v)
	}
	return &tracepb.AttributeValue{
		Value: &tracepb.AttributeValue_IntValue{IntValue: iv},
	}
}

// SpanData is a span data.
type SpanData struct {
	Name   string
	Start  time.Time
	End    time.Time
	Attrs  map[string]any
	Status *spb.Status
}

// NameKind returns span name's kind.
// It drops resource capacity for semaphore span.
func (sd SpanData) NameKind() string {
	name, _, _ := strings.Cut(sd.Name, "/")
	return name
}

// Duration returns duration of the span.
func (sd SpanData) Duration() time.Duration {
	return sd.End.Sub(sd.Start)
}
