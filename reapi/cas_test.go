// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/reapi/digest"
)

// errReader returns err on every Read.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestExpectEOF(t *testing.T) {
	sentinel := errors.New("read failed")
	for _, tc := range []struct {
		name     string
		r        io.Reader
		wantEOF  bool  // expectEOF should report a clean EOF (nil)
		wantErrs error // if non-nil, expectEOF's error must match via errors.Is
	}{
		{
			name:    "clean EOF",
			r:       bytes.NewReader(nil),
			wantEOF: true,
		},
		{
			name: "trailing data",
			r:    bytes.NewReader([]byte("x")),
		},
		{
			name:     "read error",
			r:        errReader{err: sentinel},
			wantErrs: sentinel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := expectEOF(tc.r)
			switch {
			case tc.wantEOF:
				if err != nil {
					t.Errorf("expectEOF=%v; want nil", err)
				}
			case tc.wantErrs != nil:
				if !errors.Is(err, tc.wantErrs) {
					t.Errorf("expectEOF=%v; want %v", err, tc.wantErrs)
				}
			default:
				if err == nil {
					t.Errorf("expectEOF=nil; want trailing-data error")
				}
			}
		})
	}
}

func TestCreateBatchUpdateBlobsRequests(t *testing.T) {
	ctx := t.Context()
	rnd := rand.NewChaCha8([32]byte{})
	ds := digest.NewStore()
	testdata := func(s string, n int64) digest.Data {
		buf := make([]byte, n)
		rnd.Read(buf)
		return digest.FromBytes(s, buf)
	}
	ds.Set(testdata("data 0", 512*1024))
	for i := 1; i < 15; i++ {
		ds.Set(testdata(fmt.Sprintf("data %d", i), 1024*1024))
	}
	uploadOps := make(map[digest.Digest]*uploadOp)
	for _, d := range ds.List() {
		uploadOps[d] = newUploadOp()
	}
	sizeLimit := int64(10 * 1024 * 1024)
	numLimit := 10
	blobsReqs, missingBlobs := blobsToUpload(ctx, ds.List(), ds, sizeLimit)
	batchReqs := createBatchUpdateBlobsRequests("projects/test/instances/default_instannce", blobsReqs, sizeLimit, numLimit)
	nBatches := 0
	for batchReq := range batchReqs {
		size := int64(proto.Size(batchReq))
		num := len(batchReq.Requests)
		t.Logf("size=%d num=%d", size, num)
		if size > sizeLimit || num > numLimit {
			t.Errorf("size=%d num=%d; exceeds limit size=%d num=%d", size, num, sizeLimit, numLimit)
		}
		nBatches++
	}
	if nBatches != 2 {
		t.Errorf("too many batch requests %d; want 2", nBatches)
	}
	if m := missingBlobs.Size(); m != 0 {
		t.Errorf("missingBlobs=%d; want=0", m)
	}
}

// newZstdTestClient builds a Client wired for newDecoder testing
// only: just the zstd decoder pool and an Option that selects ZSTD
// for any blob size. It is not safe for any RPC-using method.
func newZstdTestClient() *Client {
	pool := &sync.Pool{}
	pool.New = func() any {
		dec, err := zstd.NewReader(nil, zstdDecoderOpts...)
		if err != nil {
			panic(err)
		}
		return &pooledDecoder{Decoder: dec, pool: pool}
	}
	return &Client{
		opt: Option{
			compressor:     rpb.Compressor_ZSTD,
			CompressedBlob: 0,
		},
		zstdDecoderPool: pool,
	}
}

func zstdEncode(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	enc, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatalf("encode write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("encode close: %v", err)
	}
	return out.Bytes()
}

// zstdBlobOfCompressedSize returns a valid zstd stream of exactly
// target bytes plus the payload it decompresses to. A small data
// frame is followed by a skippable frame padded to the requested
// total length.
func zstdBlobOfCompressedSize(t *testing.T, target int, seed [32]byte) (compressed, payload []byte) {
	t.Helper()
	rnd := rand.NewChaCha8(seed)
	payload = make([]byte, 64)
	rnd.Read(payload)
	base := zstdEncode(t, payload)
	if len(base) > target {
		t.Fatalf("base compressed %d > target %d", len(base), target)
	}
	pad := target - len(base)
	if pad != 0 && pad < 8 {
		t.Fatalf("padding %d cannot fit a skippable frame header (8 bytes)", pad)
	}
	if pad == 0 {
		return base, payload
	}
	frame := make([]byte, pad)
	// 0x184D2A50 is one of zstd's skippable-frame magic numbers; the
	// decoder skips the frame's contents.
	binary.LittleEndian.PutUint32(frame[0:4], 0x184D2A50)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(pad-8))
	rnd.Read(frame[8:])
	return append(base, frame...), payload
}

// Random data; compressed bytes stay under probeSize.
func TestNewDecoder_ZSTDPreBufferRoundTrip(t *testing.T) {
	c := newZstdTestClient()
	rnd := rand.NewChaCha8([32]byte{1})
	payload := make([]byte, probeSize/4)
	rnd.Read(payload)
	compressed := zstdEncode(t, payload)

	d := digest.Digest{Hash: "test-prebuffer", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	t.Cleanup(func() { rd.Close() })

	if _, ok := rd.(*bufferedDecoder); !ok {
		t.Errorf("got %T; want *bufferedDecoder for blob below probe", rd)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decoded mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// Random data; compressed bytes exceed probeSize so the MultiReader path streams.
func TestNewDecoder_ZSTDStreamingRoundTrip(t *testing.T) {
	c := newZstdTestClient()
	rnd := rand.NewChaCha8([32]byte{2})
	payload := make([]byte, freshDecoderThreshold+probeSize)
	rnd.Read(payload)
	compressed := zstdEncode(t, payload)

	d := digest.Digest{Hash: "test-streaming", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	t.Cleanup(func() { rd.Close() })

	if _, ok := rd.(*discardableDecoder); !ok {
		t.Errorf("got %T; want *discardableDecoder for blob above probe", rd)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decoded mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// Highly compressible blob: compressed bytes fit in the probe, but
// decompressed size meets freshDecoderThreshold. Must take the
// discardable path so the decoder's syncStream.dstBuf, which holds
// the entire decoded output after sync DecodeAll, can't end up in
// the pool.
func TestNewDecoder_ZSTDHighlyCompressibleLargeBlob(t *testing.T) {
	c := newZstdTestClient()
	payload := make([]byte, freshDecoderThreshold+probeSize)
	compressed := zstdEncode(t, payload)
	if len(compressed) > probeSize {
		t.Fatalf("expected compressed bytes <= probeSize; got %d", len(compressed))
	}

	d := digest.Digest{Hash: "test-compressible-large", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	t.Cleanup(func() { rd.Close() })

	if _, ok := rd.(*discardableDecoder); !ok {
		t.Errorf("got %T; want *discardableDecoder for compressed<=probe but decoded>=threshold", rd)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decoded mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// Discardable decoders must drop, not pool: window-sized h.b would
// poison the pool. sync.Pool exposes no Put hook, so this test uses
// an indirect proxy: pooledDecoder.Close calls Reset(nil), leaving
// the underlying *zstd.Decoder usable; discardableDecoder.Close calls
// Decoder.Close, after which Reset(nil) returns ErrDecoderClosed.
func TestNewDecoder_DiscardableDoesNotPool(t *testing.T) {
	c := newZstdTestClient()
	rnd := rand.NewChaCha8([32]byte{3})
	payload := make([]byte, freshDecoderThreshold+probeSize)
	rnd.Read(payload)
	compressed := zstdEncode(t, payload)

	d := digest.Digest{Hash: "test-no-pool", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	sd, ok := rd.(*discardableDecoder)
	if !ok {
		t.Fatalf("got %T; want *discardableDecoder", rd)
	}
	underlying := sd.Decoder

	if _, err := io.ReadAll(rd); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := underlying.Reset(nil); err == nil {
		t.Errorf("discardable decoder usable after Close; expected ErrDecoderClosed (would mean it was pooled, not dropped)")
	}
}

// Sync-path decoder must remain reusable after Close (i.e., return
// to the pool, not be dropped). sync.Pool exposes no Put hook, so
// this test uses the same indirect proxy as TestNewDecoder_DiscardableDoesNotPool:
// after Close, Reset(nil) on the underlying *zstd.Decoder must succeed
// (a Closed decoder would return ErrDecoderClosed).
func TestNewDecoder_PreBufferReturnsToPool(t *testing.T) {
	c := newZstdTestClient()
	payload := []byte("small payload, decoder should be pooled back")
	compressed := zstdEncode(t, payload)

	d := digest.Digest{Hash: "test-pool", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	bd, ok := rd.(*bufferedDecoder)
	if !ok {
		t.Fatalf("got %T; want *bufferedDecoder", rd)
	}
	underlying := bd.Decoder

	if _, err := io.ReadAll(rd); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := underlying.Reset(nil); err != nil {
		t.Errorf("pre-buffer decoder Closed after Close; should remain pool-usable: %v", err)
	}
}

func TestNewDecoder_IdentityPassthrough(t *testing.T) {
	c := newZstdTestClient()
	c.opt.compressor = rpb.Compressor_IDENTITY

	payload := []byte("hello world")
	d := digest.Digest{Hash: "test-identity", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(payload), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	t.Cleanup(func() { rd.Close() })

	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("passthrough mismatch: got %q, want %q", got, payload)
	}
}

func TestBufferedDecoder_CloseReleasesBuffer(t *testing.T) {
	c := newZstdTestClient()
	payload := []byte("small payload for lifetime check")
	compressed := zstdEncode(t, payload)

	d := digest.Digest{Hash: "test-lifetime", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	bd, ok := rd.(*bufferedDecoder)
	if !ok {
		t.Fatalf("got %T; want *bufferedDecoder", rd)
	}
	if bd.buf == nil {
		t.Fatal("bufferedDecoder.buf is nil before Close")
	}
	if _, err := io.ReadAll(rd); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bd.buf != nil {
		t.Errorf("bufferedDecoder.buf not nilled by Close; got %p", bd.buf)
	}
}

// A blob whose compressed length equals probeSize must take the sync
// path. CopyN with a probeSize limit would never see EOF on a reader
// holding exactly that many bytes; reading probeSize+1 catches it.
func TestNewDecoder_ZSTDExactProbeSize(t *testing.T) {
	c := newZstdTestClient()
	compressed, payload := zstdBlobOfCompressedSize(t, probeSize, [32]byte{99})
	if len(compressed) != probeSize {
		t.Fatalf("blob length %d != probeSize %d", len(compressed), probeSize)
	}

	d := digest.Digest{Hash: "test-exact-probe", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	t.Cleanup(func() { rd.Close() })

	if _, ok := rd.(*bufferedDecoder); !ok {
		t.Errorf("got %T; want *bufferedDecoder for compressed size = probeSize", rd)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decoded mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// A blob one byte over probeSize must take the streaming branch. With
// d.SizeBytes below freshDecoderThreshold it shares a pool decoder
// (bufferedDecoder); the *discardableDecoder fresh-decoder path is
// covered by TestNewDecoder_ZSTDStreamingRoundTrip.
func TestNewDecoder_ZSTDProbeSizePlusOne(t *testing.T) {
	c := newZstdTestClient()
	compressed, payload := zstdBlobOfCompressedSize(t, probeSize+1, [32]byte{100})
	if len(compressed) != probeSize+1 {
		t.Fatalf("blob length %d != probeSize+1 %d", len(compressed), probeSize+1)
	}

	d := digest.Digest{Hash: "test-probe-plus-one", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	t.Cleanup(func() { rd.Close() })

	if _, ok := rd.(*bufferedDecoder); !ok {
		t.Errorf("got %T; want *bufferedDecoder for streaming via pool decoder", rd)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decoded mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// errAfterReader returns data then a custom error on the next Read.
// Used to drive the io.CopyN error branch in newDecoder.
type errAfterReader struct {
	data []byte
	err  error
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// A non-EOF error from the source mid-probe must propagate from
// newDecoder. Probe buf is recycled even on this path.
func TestNewDecoder_ZSTDProbeReadError(t *testing.T) {
	c := newZstdTestClient()
	payload := []byte("payload that won't be reached")
	compressed := zstdEncode(t, payload)
	wantErr := errors.New("synthetic mid-probe read error")
	r := &errAfterReader{data: compressed[:len(compressed)/2], err: wantErr}

	d := digest.Digest{Hash: "test-probe-error", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(r, d)
	if rd != nil {
		t.Errorf("got non-nil decoder %T on error path", rd)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("got err=%v; want %v", err, wantErr)
	}
}

// Close must be idempotent: callers may double-Close in error/defer
// paths and a second call must not double-Put the pooled decoder or
// NPE on the recycled buf.
func TestNewDecoder_DoubleCloseBuffered(t *testing.T) {
	c := newZstdTestClient()
	payload := []byte("small payload, idempotent Close check")
	compressed := zstdEncode(t, payload)

	d := digest.Digest{Hash: "test-double-close-buffered", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	if _, ok := rd.(*bufferedDecoder); !ok {
		t.Fatalf("got %T; want *bufferedDecoder", rd)
	}
	if _, err := io.ReadAll(rd); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Errorf("second Close should be a safe no-op; got %v", err)
	}
}

func TestNewDecoder_DoubleCloseDiscardable(t *testing.T) {
	c := newZstdTestClient()
	rnd := rand.NewChaCha8([32]byte{42})
	payload := make([]byte, freshDecoderThreshold+probeSize)
	rnd.Read(payload)
	compressed := zstdEncode(t, payload)

	d := digest.Digest{Hash: "test-double-close-discardable", SizeBytes: int64(len(payload))}
	rd, err := c.newDecoder(bytes.NewReader(compressed), d)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	if _, ok := rd.(*discardableDecoder); !ok {
		t.Fatalf("got %T; want *discardableDecoder", rd)
	}
	if _, err := io.ReadAll(rd); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Errorf("second Close should be a safe no-op; got %v", err)
	}
}
