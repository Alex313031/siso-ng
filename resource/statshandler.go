// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resource

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/stats"
)

// StatsHandler is a grpc/stats.Handler that feeds a Network gate with
// the time to first byte of each download RPC: Begin -> first
// InPayload. Measured to InPayload rather than InHeader so a server
// that returns headers fast but stalls on the body does not look
// healthy.
//
// Only RPCs whose method name matches one of the download method
// suffixes contribute samples; control-plane calls are excluded.
type StatsHandler struct {
	g       *Network
	methods []string
}

// NewStatsHandler filters to the ByteStream.Read and BatchReadBlobs
// methods by default.
func NewStatsHandler(g *Network) *StatsHandler {
	return &StatsHandler{
		g: g,
		methods: []string{
			"/google.bytestream.ByteStream/Read",
			"/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchReadBlobs",
		},
	}
}

type rpcStateKey struct{}

type rpcState struct {
	begin        time.Time
	firstPayload bool
}

// TagRPC attaches per-RPC state only for the download methods we sample;
// excluded control-plane RPCs pass through with no allocation.
func (h *StatsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	for _, m := range h.methods {
		if strings.HasSuffix(info.FullMethodName, m) || info.FullMethodName == m {
			return context.WithValue(ctx, rpcStateKey{}, &rpcState{})
		}
	}
	return ctx
}

func (h *StatsHandler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	state, _ := ctx.Value(rpcStateKey{}).(*rpcState)
	if state == nil {
		return
	}
	switch ev := rs.(type) {
	case *stats.Begin:
		state.begin = ev.BeginTime
	case *stats.InPayload:
		if !state.firstPayload {
			state.firstPayload = true
			if !state.begin.IsZero() {
				h.g.observeTTFB(ev.RecvTime.Sub(state.begin))
			}
		}
	}
}

func (h *StatsHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h *StatsHandler) HandleConn(context.Context, stats.ConnStats) {}
