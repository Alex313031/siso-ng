// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package monitoring

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/stats"
)

// PeerMaxConcurrentStreams is the per-conn HTTP/2 stream cap RBE
// advertises in its SETTINGS frame. Constant because gRPC-Go does not
// expose the value via public APIs.
const PeerMaxConcurrentStreams int64 = 100

// Populated by SetupViews. Recording into a nil instrument is a
// no-op so the stats handler is safe to invoke before init.
var (
	bsTTFB          metric.Float64Histogram
	bsTransportPick metric.Float64Histogram
	bsClientQueue   metric.Float64Histogram
	bsServerSetup   metric.Float64Histogram
	bsServerFetch   metric.Float64Histogram
	bsBodyDownload  metric.Float64Histogram
)

// bsHandler is the singleton stats handler. Per-RPC state lives on
// the ctx returned by TagRPC.
var bsHandler = &bytestreamStatsHandler{}

// BytestreamStatsHandler returns the stats.Handler backing the
// bytestream.* metrics. Wire via grpc.WithStatsHandler.
func BytestreamStatsHandler() stats.Handler { return bsHandler }

// connState tracks active streams on one TCP conn, keyed by local
// address (uniquely identifies the conn from this process). peak is a
// high-water mark updated on every stream-start; the OTel callback
// resets it on read so each export reports the peak observed within
// the export window rather than an instantaneous sampling artifact.
type connState struct {
	active atomic.Int64
	peak   atomic.Int64
}

const bytestreamReadMethod = "/google.bytestream.ByteStream/Read"

// rpcState carries per-RPC stage timestamps, filled in by HandleRPC.
type rpcState struct {
	method        string       // FullMethodName captured from RPCTagInfo
	begin         atomic.Int64 // unix nanos at stats.Begin
	queueStart    atomic.Int64 // unix nanos the client_queue phase starts; reset to the transport pick time once a delayed pick completes
	transportPick atomic.Int64 // begin until DelayedPickComplete, set only when a pick delay occurred
	outHeader     atomic.Int64
	inHeader      atomic.Int64
	inPayload     atomic.Int64
	// localAddr: set at OutHeader, used to decrement perConn at End.
	localAddr atomic.Pointer[string]
	// remoteAddr: set at OutHeader, attached as server.address on
	// stage histograms so dashboards can break down by VIP.
	remoteAddr atomic.Pointer[string]
}

type rpcStateKey struct{}

type bytestreamStatsHandler struct {
	// perConn maps localAddr -> *connState. Never evicted; bounded by
	// live TCP conns (~hundreds).
	perConn sync.Map
}

func (h *bytestreamStatsHandler) stateFor(addr string) *connState {
	if v, ok := h.perConn.Load(addr); ok {
		return v.(*connState)
	}
	v, _ := h.perConn.LoadOrStore(addr, &connState{})
	return v.(*connState)
}

// MaxActiveStreams is the highest active-stream count across live
// subconns at this instant. Approaching PeerMaxConcurrentStreams means
// we're about to queue inside gRPC's transport.
func (h *bytestreamStatsHandler) MaxActiveStreams() int64 {
	var max int64
	h.perConn.Range(func(_, v any) bool {
		if n := v.(*connState).active.Load(); n > max {
			max = n
		}
		return true
	})
	return max
}

// PeakActiveStreamsSinceReset returns the highest active-stream count
// observed across live subconns since the last call, then resets each
// sub's peak to the current active. Used by the OTel observable gauge
// so the metric captures the peak between exports rather than just an
// instantaneous sampling snapshot - brief saturation spikes that fall
// between 10s scrapes can't hide. Uses current active as a floor so a
// concurrent start that skipped updating peak (saw the old high-water
// mark) can't drop the observation below what's actually live.
func (h *bytestreamStatsHandler) PeakActiveStreamsSinceReset() int64 {
	var max int64
	h.perConn.Range(func(_, v any) bool {
		cs := v.(*connState)
		cur := cs.active.Load()
		p := cs.peak.Swap(cur)
		if cur > p {
			p = cur
		}
		if p > max {
			max = p
		}
		return true
	})
	return max
}

func (h *bytestreamStatsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	s := &rpcState{}
	if info != nil {
		s.method = info.FullMethodName
	}
	return context.WithValue(ctx, rpcStateKey{}, s)
}

func (h *bytestreamStatsHandler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	s, ok := ctx.Value(rpcStateKey{}).(*rpcState)
	if !ok {
		return
	}
	h.trackActiveStreams(s, rs)
	if s.method == bytestreamReadMethod {
		h.recordReadStages(ctx, s, rs)
	}
}

// trackActiveStreams maintains the per-conn active-stream counter for
// every RPC on the conn; saturation is a connection-level property and
// applies to ByteStream.Read and the unary REAPI calls equally. Also
// updates the per-conn peak high-water mark so the OTel gauge can
// report between-scrape peaks instead of instantaneous samples.
func (h *bytestreamStatsHandler) trackActiveStreams(s *rpcState, rs stats.RPCStats) {
	switch v := rs.(type) {
	case *stats.OutHeader:
		if v.LocalAddr != nil && s.localAddr.Load() == nil {
			addr := v.LocalAddr.String()
			if s.localAddr.CompareAndSwap(nil, &addr) {
				cs := h.stateFor(addr)
				n := cs.active.Add(1)
				for {
					p := cs.peak.Load()
					if n <= p {
						break
					}
					if cs.peak.CompareAndSwap(p, n) {
						break
					}
				}
			}
		}
		if v.RemoteAddr != nil && s.remoteAddr.Load() == nil {
			host, _, err := net.SplitHostPort(v.RemoteAddr.String())
			if err != nil {
				host = v.RemoteAddr.String()
			}
			s.remoteAddr.CompareAndSwap(nil, &host)
		}
	case *stats.End:
		if ap := s.localAddr.Load(); ap != nil {
			h.stateFor(*ap).active.Add(-1)
		}
	}
}

// recordReadStages stamps per-stage timestamps for ByteStream.Read and
// emits stage histograms at End. Called only for that method so the
// bytestream.read.* series stays clean of unary REAPI traffic.
func (h *bytestreamStatsHandler) recordReadStages(ctx context.Context, s *rpcState, rs stats.RPCStats) {
	now := time.Now().UnixNano()
	switch rs.(type) {
	case *stats.Begin:
		s.begin.Store(now)
		s.queueStart.Store(now)
	case *stats.DelayedPickComplete:
		// gRPC delayed selecting a ready SubConn/transport. Charge
		// begin..now to transport_pick and restart the client_queue
		// phase here so it is not double counted.
		if begin := s.begin.Load(); begin > 0 && now > begin {
			s.transportPick.Store(now - begin)
		}
		s.queueStart.Store(now)
	case *stats.OutHeader:
		s.outHeader.Store(now)
	case *stats.InHeader:
		s.inHeader.Store(now)
	case *stats.InPayload:
		// First decoded ReadResponse. stats.InPayload fires per gRPC
		// message (post-RecvMsg), not per HTTP/2 DATA frame, so this is
		// first-message-decoded latency, not strictly first-byte.
		s.inPayload.CompareAndSwap(0, now)
	case *stats.End:
		h.recordStages(ctx, s, now)
	}
}

func (h *bytestreamStatsHandler) recordStages(ctx context.Context, s *rpcState, end int64) {
	begin := s.begin.Load()
	queueStart := s.queueStart.Load()
	if queueStart == 0 {
		queueStart = begin
	}
	outH := s.outHeader.Load()
	inH := s.inHeader.Load()
	inP := s.inPayload.Load()
	var perCall []attribute.KeyValue
	if rp := s.remoteAddr.Load(); rp != nil {
		perCall = []attribute.KeyValue{attribute.String("server.address", *rp)}
	}
	// Skip stages that never fired (aborted RPCs may not reach InHeader/InPayload).
	if d := s.transportPick.Load(); d > 0 {
		recordMs(ctx, bsTransportPick, d, perCall)
	}
	if queueStart > 0 && outH > 0 {
		recordMs(ctx, bsClientQueue, outH-queueStart, perCall)
	}
	if outH > 0 && inH > 0 {
		recordMs(ctx, bsServerSetup, inH-outH, perCall)
	}
	if inH > 0 && inP > 0 {
		recordMs(ctx, bsServerFetch, inP-inH, perCall)
	}
	if inP > 0 && end > inP {
		recordMs(ctx, bsBodyDownload, end-inP, perCall)
	}
	// begin -> first decoded ReadResponse from server (see recordReadStages).
	if begin > 0 && inP > 0 {
		recordMs(ctx, bsTTFB, inP-begin, perCall)
	}
}

func recordMs(ctx context.Context, h metric.Float64Histogram, ns int64, extra []attribute.KeyValue) {
	if h == nil {
		return
	}
	// Mirror staticMetricLabels so dashboards filtering on
	// siso_version / os_family pick up these series; extra carries
	// per-call attributes (e.g. server.address).
	attrs := make([]attribute.KeyValue, 0, len(staticMetricLabels)+len(extra))
	attrs = append(attrs, staticMetricLabels...)
	attrs = append(attrs, extra...)
	h.Record(ctx, float64(ns)/1e6, metric.WithAttributes(attrs...))
}

func (h *bytestreamStatsHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h *bytestreamStatsHandler) HandleConn(_ context.Context, _ stats.ConnStats) {}

// setupBytestreamMetrics registers OTel instruments. Called from SetupViews.
func setupBytestreamMetrics() error {
	var err error

	_, err = meter.Int64ObservableGauge(
		"bytestream.subconn.peak_active_streams",
		metric.WithDescription("Peak active HTTP/2 streams observed across all live subconns since the previous metric scrape (high-water mark, reset on read). Catches brief saturation spikes between scrapes."),
		metric.WithUnit("{stream}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(bsHandler.PeakActiveStreamsSinceReset(), metric.WithAttributes(staticMetricLabels...))
			return nil
		}),
	)
	if err != nil {
		return err
	}

	bsTTFB, err = meter.Float64Histogram(
		"bytestream.read.ttfb",
		metric.WithDescription("Time from RPC begin to first decoded ByteStream.ReadResponse. Per-message granularity (gRPC stats.InPayload), not per-HTTP/2-frame."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	bsTransportPick, err = meter.Float64Histogram(
		"bytestream.read.transport_pick",
		metric.WithDescription("Time a ByteStream.Read RPC spent waiting for gRPC to select a ready SubConn/transport before stream allocation. Recorded only when the pick was delayed."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	bsClientQueue, err = meter.Float64Histogram(
		"bytestream.read.client_queue",
		metric.WithDescription("Time between gRPC transport pick completion and HEADERS frame going on the wire. Non-zero values indicate the client-side stream queue (MAX_CONCURRENT_STREAMS reached)."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	bsServerSetup, err = meter.Float64Histogram(
		"bytestream.read.server_setup",
		metric.WithDescription("Time between HEADERS going on the wire and the server's initial response headers arriving."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	bsServerFetch, err = meter.Float64Histogram(
		"bytestream.read.server_fetch",
		metric.WithDescription("Time between server's initial response headers and the first decoded ReadResponse. Includes backend blob-fetch plus first-message transmission and decode."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	bsBodyDownload, err = meter.Float64Histogram(
		"bytestream.read.body_download",
		metric.WithDescription("Time from first decoded ReadResponse to RPC end. Excludes the first message's transmission/decode (folded into server_fetch)."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	return nil
}

// bytestreamHistogramBuckets: fine resolution sub-second, finer
// around the dense 500ms-2s region where ttfb/server_setup p50-p95
// concentrate, coarse into the alarming tail. Anything >30s is
// "broken" and doesn't need sub-bucket resolution.
var bytestreamHistogramBuckets = []float64{
	1, 5, 10, 25, 50, 100, 200, 500, 750,
	1000, 1500, 2000, 5000, 10000, 30000,
}
