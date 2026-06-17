// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package firstbyte exposes a gRPC stats handler that closes a
// caller-supplied Signal on the first stats.InPayload for any RPC
// tagged against a ctx from WithSignal. stats.InPayload fires after
// gRPC RecvMsg has decoded a full response message, so the Signal
// detects "first decoded message", not strictly first wire byte; on a
// slow link delivering a multi-MiB ReadResponse, bytes may flow before
// the Signal fires. For ByteStream.Read this is still strictly better
// than user-space read progress, which lags decoding further.
//
// Typical use:
//
//	ctx, sig := firstbyte.WithSignal(ctx)
//	go func() {
//	    select {
//	    case <-sig.Fired():
//	    case <-time.After(timeout):
//	        cancel("no wire data within ...")
//	    }
//	}()
//	r, err := source.Open(ctx)
//
// Register with grpc.WithStatsHandler(firstbyte.Handler).
package firstbyte

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// Signal is the one-shot first-byte notification.
type Signal struct {
	fired chan struct{}
	once  sync.Once
}

// Fired returns a channel that closes on the first observed InPayload.
func (s *Signal) Fired() <-chan struct{} { return s.fired }

// signal closes the channel; idempotent.
func (s *Signal) signal() {
	s.once.Do(func() { close(s.fired) })
}

type signalCtxKey struct{}

// WithSignal returns a ctx carrying a Signal that the handler closes
// on the first InPayload of any RPC tagged against it.
func WithSignal(ctx context.Context) (context.Context, *Signal) {
	sig := &Signal{fired: make(chan struct{})}
	return context.WithValue(ctx, signalCtxKey{}, sig), sig
}

// signalFromCtx returns the Signal on ctx or nil.
func signalFromCtx(ctx context.Context) *Signal {
	if s, ok := ctx.Value(signalCtxKey{}).(*Signal); ok {
		return s
	}
	return nil
}

type rpcState struct {
	sig *Signal
}

type rpcStateKey struct{}

type handler struct{}

// TagRPC stashes the Signal (if any) so HandleRPC can fire it without
// re-walking ctx. No-Signal ctxs pass through unchanged.
func (handler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	sig := signalFromCtx(ctx)
	if sig == nil {
		return ctx
	}
	return context.WithValue(ctx, rpcStateKey{}, &rpcState{sig: sig})
}

func (handler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	if _, ok := rs.(*stats.InPayload); !ok {
		return
	}
	s, ok := ctx.Value(rpcStateKey{}).(*rpcState)
	if !ok {
		return
	}
	s.sig.signal()
}

func (handler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (handler) HandleConn(context.Context, stats.ConnStats) {}

// Handler is the singleton stats.Handler backing the Signal
// mechanism. Method-agnostic: any RPC reaching InPayload counts as
// "data flowing".
var Handler stats.Handler = handler{}

// ErrNoFirstByte is the cause the watchdog cancels with when no first byte arrives in time.
var ErrNoFirstByte = errors.New("no first byte")

// watchdogError wraps ErrNoFirstByte and reports codes.Aborted via GRPCStatus.
type watchdogError struct {
	label   string
	timeout time.Duration
	elapsed time.Duration
}

func (e *watchdogError) Error() string {
	return fmt.Sprintf("%s %v in %s: %s", e.label, ErrNoFirstByte, e.timeout, e.elapsed)
}

func (e *watchdogError) Unwrap() error { return ErrNoFirstByte }

func (e *watchdogError) GRPCStatus() *status.Status {
	return status.New(codes.Aborted, e.Error())
}

// Watchdog runs f on a derived ctx that is cancelled with codes.Aborted
// if no gRPC InPayload event fires within timeout. Returns f's error,
// or the watchdog's Aborted cause if it fired. Pair with retry.Do for
// the cancelled attempt to be retried on a fresh stream:
//
//	err := retry.Do(ctx, func() error {
//	    return firstbyte.Watchdog(ctx, 5*time.Second, "GetActionResult", func(ctx context.Context) error {
//	        _, err := client.GetActionResult(ctx, req)
//	        return err
//	    })
//	})
//
// timeout <= 0 disables the watchdog (f runs against ctx unchanged).
func Watchdog(ctx context.Context, timeout time.Duration, label string, f func(context.Context) error) error {
	if timeout <= 0 {
		return f(ctx)
	}
	ctx, sig := WithSignal(ctx)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	started := time.Now()
	go func() {
		select {
		case <-sig.Fired():
		case <-ctx.Done():
		case <-time.After(timeout):
			cancel(&watchdogError{label: label, timeout: timeout, elapsed: time.Since(started)})
		}
	}()
	err := f(ctx)
	if ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	return err
}
