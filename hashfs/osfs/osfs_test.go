// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package osfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/firstbyte"
)

// stallingSource is a digest.Source whose Reader.Read blocks until the
// per-call ctx is cancelled. Models a wedged ByteStream Read where the
// server has accepted the stream but no DATA frames are arriving.
type stallingSource struct{}

func (stallingSource) Open(ctx context.Context) (io.ReadCloser, error) {
	return &stallingReader{ctx: ctx}, nil
}

func (stallingSource) String() string { return "stalling-source" }

type stallingReader struct {
	ctx context.Context
}

func (r *stallingReader) Read(_ []byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *stallingReader) Close() error { return nil }

// promptSource returns the given content immediately on the first
// Read. Used to verify the pre-first-byte watchdog stands down once
// data starts flowing.
type promptSource struct{ body []byte }

func (p promptSource) Open(_ context.Context) (io.ReadCloser, error) {
	return &promptReader{body: p.body}, nil
}
func (promptSource) String() string { return "prompt-source" }

type promptReader struct {
	body []byte
	pos  int
}

func (r *promptReader) Read(buf []byte) (int, error) {
	if r.pos >= len(r.body) {
		return 0, io.EOF
	}
	n := copy(buf, r.body[r.pos:])
	r.pos += n
	return n, nil
}
func (r *promptReader) Close() error { return nil }

func TestWriteDigestData_FirstByteTimeout_Fires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ofs := New(t.Context(), "test", Option{})
		target := filepath.Join(t.TempDir(), "out.bin")
		ctx := ofs.WithFirstByteTimeout(t.Context(), 100*time.Millisecond)

		// timeout=10s so the no-ops watchdog can't be the one to fire.
		err := ofs.WriteDigestData(ctx, target, stallingSource{}, 0o600, 10*time.Second)
		if err == nil {
			t.Fatalf("WriteDigestData: nil err on stalled source")
		}
		if !strings.Contains(err.Error(), "no first byte in 100ms") {
			t.Errorf("err did not name the pre-first-byte watchdog: %v", err)
		}
		st, ok := status.FromError(unwrap(err))
		if !ok {
			t.Fatalf("error is not a status: %v", err)
		}
		if st.Code() != codes.Aborted {
			t.Errorf("status code = %v, want Aborted (retryable)", st.Code())
		}
	})
}

func TestWriteDigestData_FirstByteTimeout_DoesNotFireWhenDataFlows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ofs := New(t.Context(), "test", Option{})
		target := filepath.Join(t.TempDir(), "out.bin")
		ctx := ofs.WithFirstByteTimeout(t.Context(), 50*time.Millisecond)
		body := []byte("hello world")
		err := ofs.WriteDigestData(ctx, target, promptSource{body: body}, 0o600, 10*time.Second)
		if err != nil {
			t.Fatalf("WriteDigestData: %v", err)
		}
	})
}

// signalingSource fires firstbyte.Handler's InPayload inside Open
// (mimicking a decoder that observed the first DATA frame), then
// serves the body lazily.
type signalingSource struct{ body []byte }

func (s signalingSource) Open(ctx context.Context) (io.ReadCloser, error) {
	h := firstbyte.Handler
	rpcCtx := h.TagRPC(ctx, &stats.RPCTagInfo{FullMethodName: "/google.bytestream.ByteStream/Read"})
	h.HandleRPC(rpcCtx, &stats.InPayload{}) // closes firstbyte.Signal
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

func (signalingSource) String() string { return "signaling-source" }

// Watchdog stands down on gRPC InPayload, not user-space Read; a
// source firing InPayload inside Open keeps it quiet.
func TestWriteDigestData_FirstByteSignal_StandsDownWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ofs := New(t.Context(), "test", Option{})
		target := filepath.Join(t.TempDir(), "out.bin")
		// 25ms watchdog; signal fires inside Open, so watchdog must not fire.
		ctx := ofs.WithFirstByteTimeout(t.Context(), 25*time.Millisecond)
		body := []byte("hello world")
		err := ofs.WriteDigestData(ctx, target, signalingSource{body: body}, 0o600, 10*time.Second)
		if err != nil {
			t.Fatalf("WriteDigestData: %v", err)
		}
	})
}

// No WithFirstByteTimeout on ctx -> no pre-first-byte watchdog; the
// no-ops watchdog at the caller-supplied timeout is the only one armed.
func TestWriteDigestData_FirstByteTimeout_Disabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ofs := New(t.Context(), "test", Option{})
		target := filepath.Join(t.TempDir(), "out.bin")
		err := ofs.WriteDigestData(t.Context(), target, stallingSource{}, 0o600, 100*time.Millisecond)
		if err == nil {
			t.Fatalf("WriteDigestData: nil err with both watchdogs nominally active")
		}
		if strings.Contains(err.Error(), "no first byte") {
			t.Errorf("watchdog not opted in but pre-first-byte fired: %v", err)
		}
		if !strings.Contains(err.Error(), "no ops in 100ms") {
			t.Errorf("expected existing no-ops watchdog to fire, got: %v", err)
		}
	})
}

// slowCtxIgnoringSource models a local FileSource that ignores ctx
// (file.Read does not honor ctx) and serves its body slowly enough for
// the pre-first-byte watchdog to fire mid-copy. Used to verify a
// successful copy is not turned into Aborted just because a watchdog
// cancelled ctx in the background.
type slowCtxIgnoringSource struct {
	body  []byte
	delay time.Duration
}

func (s slowCtxIgnoringSource) Open(_ context.Context) (io.ReadCloser, error) {
	return &slowReader{body: s.body, delay: s.delay}, nil
}
func (slowCtxIgnoringSource) String() string { return "slow-ctx-ignoring-source" }

type slowReader struct {
	body  []byte
	pos   int
	delay time.Duration
}

func (r *slowReader) Read(buf []byte) (int, error) {
	if r.pos >= len(r.body) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := copy(buf, r.body[r.pos:])
	r.pos += n
	return n, nil
}
func (r *slowReader) Close() error { return nil }

// 30ms watchdog, 50ms-per-read source -> watchdog fires before the
// first Read returns. Reader ignores ctx; io.Copy completes normally.
// WriteDigestData must report success.
func TestWriteDigestData_FalseAbort_LocalSourceCompletesAfterWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ofs := New(t.Context(), "test", Option{})
		target := filepath.Join(t.TempDir(), "out.bin")
		ctx := ofs.WithFirstByteTimeout(t.Context(), 30*time.Millisecond)
		body := []byte("hello world")
		src := slowCtxIgnoringSource{body: body, delay: 50 * time.Millisecond}
		err := ofs.WriteDigestData(ctx, target, src, 0o600, 10*time.Second)
		if err != nil {
			t.Fatalf("WriteDigestData: %v (watchdog cancel must not override successful copy)", err)
		}
	})
}

// cancelOnReadSource cancels the caller's ctx on first Read, then serves
// its body ignoring ctx. Models a caller giving up mid-copy while a local
// source (whose Read does not honor ctx) runs to completion.
type cancelOnReadSource struct {
	body   []byte
	cancel context.CancelCauseFunc
	cause  error
}

func (s cancelOnReadSource) Open(_ context.Context) (io.ReadCloser, error) {
	return &cancelOnReadReader{body: s.body, cancel: s.cancel, cause: s.cause}, nil
}
func (cancelOnReadSource) String() string { return "cancel-on-read-source" }

type cancelOnReadReader struct {
	body     []byte
	pos      int
	cancel   context.CancelCauseFunc
	cause    error
	canceled bool
}

func (r *cancelOnReadReader) Read(buf []byte) (int, error) {
	if !r.canceled {
		r.cancel(r.cause) // caller gives up mid-copy
		r.canceled = true
	}
	if r.pos >= len(r.body) {
		return 0, io.EOF
	}
	n := copy(buf, r.body[r.pos:])
	r.pos += n
	return n, nil
}
func (r *cancelOnReadReader) Close() error { return nil }

// A caller cancellation during a copy that still completes must surface
// as the caller's cause, not be dropped as success (else flushWrite
// renames/Chtimes after the caller gave up). No watchdog is armed here.
func TestWriteDigestData_CallerCancel_SurfacesAfterSuccessfulCopy(t *testing.T) {
	ofs := New(t.Context(), "test", Option{})
	target := filepath.Join(t.TempDir(), "out.bin")

	wantErr := errors.New("flush canceled by caller")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	src := cancelOnReadSource{body: []byte("hello world"), cancel: cancel, cause: wantErr}

	// timeout=10s so the no-ops watchdog can't fire first.
	err := ofs.WriteDigestData(ctx, target, src, 0o600, 10*time.Second)
	if err == nil {
		t.Fatalf("WriteDigestData: nil err; caller cancellation dropped after a successful copy")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want caller cause %v", err, wantErr)
	}
}

func unwrap(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}

var _ = digest.Digest{}
