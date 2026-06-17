// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package firstbyte

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// No-Signal ctx: TagRPC passthrough, HandleRPC no-op on every event.
func TestHandler_NoSignalInCtxIsNoOp(t *testing.T) {
	h := Handler
	ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{FullMethodName: "/x/y"})
	h.HandleRPC(ctx, &stats.Begin{})
	h.HandleRPC(ctx, &stats.OutHeader{})
	h.HandleRPC(ctx, &stats.InHeader{})
	h.HandleRPC(ctx, &stats.InPayload{})
	h.HandleRPC(ctx, &stats.End{})
}

func TestHandler_InPayloadClosesSignal(t *testing.T) {
	parent, sig := WithSignal(t.Context())
	h := Handler
	rpcCtx := h.TagRPC(parent, &stats.RPCTagInfo{FullMethodName: "/google.bytestream.ByteStream/Read"})
	select {
	case <-sig.Fired():
		t.Fatal("Signal fired before InPayload")
	default:
	}
	h.HandleRPC(rpcCtx, &stats.InPayload{})
	select {
	case <-sig.Fired():
	default:
		t.Fatal("Signal did not fire on InPayload")
	}
}

// Multiple InPayloads on a shared ctx must not double-close.
func TestHandler_InPayloadIdempotent(t *testing.T) {
	parent, _ := WithSignal(t.Context())
	h := Handler
	rpcCtx := h.TagRPC(parent, &stats.RPCTagInfo{FullMethodName: "/x/y"})
	h.HandleRPC(rpcCtx, &stats.InPayload{})
	h.HandleRPC(rpcCtx, &stats.InPayload{}) // must not panic
}

// Concurrent InPayloads on a shared Signal: exactly one close.
func TestHandler_ConcurrentInPayloadFiresOnce(t *testing.T) {
	parent, sig := WithSignal(t.Context())
	h := Handler
	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			rpcCtx := h.TagRPC(parent, &stats.RPCTagInfo{FullMethodName: "/x/y"})
			h.HandleRPC(rpcCtx, &stats.InPayload{})
		}()
	}
	wg.Wait()
	select {
	case <-sig.Fired():
	default:
		t.Fatal("Signal did not fire under concurrent InPayloads")
	}
}

// Non-InPayload events do not fire the signal; only InPayload does.
func TestHandler_OnlyInPayloadFires(t *testing.T) {
	parent, sig := WithSignal(t.Context())
	h := Handler
	rpcCtx := h.TagRPC(parent, &stats.RPCTagInfo{FullMethodName: "/x/y"})
	h.HandleRPC(rpcCtx, &stats.Begin{})
	h.HandleRPC(rpcCtx, &stats.OutHeader{})
	h.HandleRPC(rpcCtx, &stats.InHeader{})
	select {
	case <-sig.Fired():
		t.Fatal("Signal fired on non-InPayload events")
	default:
	}
	h.HandleRPC(rpcCtx, &stats.End{})
	select {
	case <-sig.Fired():
		t.Fatal("Signal fired on End event")
	default:
	}
}

// Watchdog returns f's error unchanged if the call completes before
// timeout (here: instantly).
func TestWatchdog_PassThroughOnImmediateReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sentinel := errors.New("inner error")
		err := Watchdog(t.Context(), 50*time.Millisecond, "test", func(context.Context) error {
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want %v", err, sentinel)
		}
	})
}

// Watchdog cancels with codes.Aborted if f blocks past timeout and the
// signal never fires.
func TestWatchdog_FiresOnStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		err := Watchdog(t.Context(), 30*time.Millisecond, "test", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if err == nil {
			t.Fatal("nil err on stalled call")
		}
		if !errors.Is(err, ErrNoFirstByte) {
			t.Errorf("err is not a watchdog cancellation: %v", err)
		}
		if got := status.Code(err); got != codes.Aborted {
			t.Errorf("status code = %v, want Aborted", got)
		}
	})
}

// Watchdog stays quiet if the firstbyte signal fires (the production
// trigger is a gRPC InPayload event picked up by Handler).
func TestWatchdog_StandsDownOnSignal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 50ms timeout; fire the signal at 10ms via TagRPC+InPayload.
		err := Watchdog(t.Context(), 50*time.Millisecond, "test", func(ctx context.Context) error {
			h := Handler
			rpcCtx := h.TagRPC(ctx, &stats.RPCTagInfo{FullMethodName: "/x/y"})
			time.Sleep(10 * time.Millisecond)
			h.HandleRPC(rpcCtx, &stats.InPayload{})
			// Stall past where the watchdog timeout would have fired.
			time.Sleep(100 * time.Millisecond)
			return nil
		})
		if err != nil {
			t.Errorf("err = %v, want nil (signal stood down the watchdog)", err)
		}
	})
}

// timeout <= 0 disables the watchdog entirely: f's ctx is the caller's
// ctx and a long-running f returns its own error/result unchanged.
func TestWatchdog_Disabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sentinel := errors.New("inner result")
		err := Watchdog(t.Context(), 0, "test", func(context.Context) error {
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want %v", err, sentinel)
		}
	})
}
