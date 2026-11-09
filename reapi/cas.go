// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"bytes"
	"cmp"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"path"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
	xsemaphore "golang.org/x/sync/semaphore"
	bpb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/bytestreamio"
	"go.chromium.org/build/siso/reapi/fetch"
	"go.chromium.org/build/siso/reapi/retry"
	"go.chromium.org/build/siso/sync/semaphore"
)

// FileSemaphore limits concurrent file access to create BatchUpdateBlobgs to protect from runtime thread exhaustion.
var FileSemaphore = semaphore.New("reapi-cas-file", runtime.GOMAXPROCS(0))

const (
	// defaultBatchUpdateByteLimit is bytes limit for cas BatchUpdateBlobs.
	defaultBatchUpdateByteLimit = 4 * 1024 * 1024

	// defaultBatchReadByteLimit is bytes limit for cas BatchReadBlobs.
	defaultBatchReadByteLimit = 4 * 1024 * 1024

	// batchBlobUploadLimit is max number of blobs in BatchUpdateBlobs.
	batchBlobUploadLimit = 1000
)

// uploadConcurrency is the per-UploadAll cap for batch and stream RPCs.
// Defaults to 1 (serial) so callers must opt into parallel upload.
func (c *Client) uploadConcurrency() int {
	if c.opt.UploadConcurrency > 0 {
		return c.opt.UploadConcurrency
	}
	return 1
}

func selectCompressor(serverSupported []rpb.Compressor_Value) rpb.Compressor_Value {
	if len(serverSupported) == 0 {
		// No compressor support.
		return rpb.Compressor_IDENTITY
	}
	for _, c := range []rpb.Compressor_Value{
		rpb.Compressor_ZSTD,
		rpb.Compressor_DEFLATE,
		rpb.Compressor_IDENTITY,
	} {
		if slices.Contains(serverSupported, c) {
			return c
		}
	}
	return rpb.Compressor_IDENTITY
}

type uploadOp struct {
	c   *Client
	d   digest.Digest
	ch  chan struct{}
	err error
}

func newUploadOp(c *Client, d digest.Digest) *uploadOp {
	return &uploadOp{
		c:  c,
		d:  d,
		ch: make(chan struct{}),
	}
}

// done completes the uploadOp if not already completed, updating knownDigests
// before closing ch so any waiting callers observe the updated knownDigests
// state upon waking up.
// Each uploadOp is owned by a single uploadNewBlobs call (completed when the
// blob finishes or in defer for any remaining blobs), so done is never called
// concurrently.
func (u *uploadOp) done(err error) {
	select {
	case <-u.ch:
		return
	default:
	}
	u.err = err
	if err == nil {
		u.c.knownDigests.CompareAndSwap(u.d, u, true)
	} else {
		u.c.knownDigests.CompareAndDelete(u.d, u)
	}
	close(u.ch)
}

func (u *uploadOp) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-u.ch:
		return u.err
	}
}

var (
	errUploadNotFinished = errors.New("upload not finished")
	errUploadNoResponse  = errors.New("upload no response")
)

// ErrClientClosed is the error upload requests fail with when the Client
// is closed while they are waiting or in flight.
var ErrClientClosed = errors.New("reapi: client closed")

// compressor returns a compressor for ByteStream Read/Write APIs.
func (c *Client) compressor(d digest.Digest) rpb.Compressor_Value {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.SizeBytes < c.opt.CompressedBlob {
		return rpb.Compressor_IDENTITY
	}
	return c.opt.compressor
}

// compressorForBatchUpdate returns a compressor for BatchUpdateBlobs API.
func (c *Client) compressorForBatchUpdate(d digest.Digest) rpb.Compressor_Value {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.SizeBytes < c.opt.BatchCompressedBlob {
		return rpb.Compressor_IDENTITY
	}
	return c.opt.compressorForBatchUpdateBlobs
}

// acceptableCompressors returns acceptable compressors for BatchReadBlobs API.
// If blob-level compression is disabled or the blob is too small, it returns nil.
func (c *Client) acceptableCompressors(d digest.Digest) []rpb.Compressor_Value {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opt.BatchCompressedBlob == 0 || d.SizeBytes < c.opt.BatchCompressedBlob {
		return nil
	}
	return []rpb.Compressor_Value{
		rpb.Compressor_ZSTD,
		rpb.Compressor_DEFLATE,
	}
}

// resourceName constructs a resource name for reading the blob identified by the digest.
// For uncompressed blob. the format is
//
//	`{instance_name}/blobs/{hash}/{size}`
//
// For compressed blob, the format is
//
//	`{instance_name}/compressed-blobs/{compressor}/{uncompressed_hash}/{uncompressed_size}`
//
// See also the API document.
// https://github.com/bazelbuild/remote-apis/blob/64cc5e9e422c93e1d7f0545a146fd84fcc0e8b47/build/bazel/remote/execution/v2/remote_execution.proto#L285-L292
func (c *Client) resourceName(d digest.Digest) string {
	if compressor := c.compressor(d); compressor != rpb.Compressor_IDENTITY {
		return path.Join(c.opt.Instance, "compressed-blobs",
			strings.ToLower(compressor.String()), c.digestFunctionSegment(),
			d.Hash, strconv.FormatInt(d.SizeBytes, 10))
	}
	return path.Join(c.opt.Instance, "blobs", c.digestFunctionSegment(),
		d.Hash, strconv.FormatInt(d.SizeBytes, 10))
}

// digestFunctionSegment returns the resource-name path segment for the client's
// digest function, or "" when REAPI omits it (sha256 and sha1, inferred by the
// server from the hash length). path.Join drops empty segments.
func (c *Client) digestFunctionSegment() string {
	return c.digestFn.ResourceNameSegment()
}

// probeSize is the largest compressed blob size that takes the sync
// DecodeAll path.
const probeSize = 128 * 1024

// freshDecoderThreshold (decompressed bytes) routes blobs at or above
// this size to a fresh, non-pooled decoder so its window-sized h.b
// (streaming) or syncStream.dstBuf (sync DecodeAll on a highly
// compressible blob) can't end up in the pool. Smaller blobs share
// pool decoders.
const freshDecoderThreshold = 4 * 1024 * 1024

// zstdDecoderOpts is the shared decoder configuration. Concurrency(1)
// avoids the per-decoder GOMAXPROCS worker fanout; siso's outer
// parallelism saturates CPUs. WithDecodeBuffersBelow(probeSize+1)
// makes the decoder take the sync path for any *bytes.Buffer with
// Len() <= probeSize.
var zstdDecoderOpts = []zstd.DOption{
	zstd.WithDecoderConcurrency(1),
	zstd.WithDecodeBuffersBelow(probeSize + 1),
}

var probeBufPool = sync.Pool{
	New: func() any { return bytes.NewBuffer(make([]byte, 0, probeSize+1)) },
}

func (c *Client) newDecoder(r io.Reader, d digest.Digest) (io.ReadCloser, error) {
	return c.newDecoderForCompressor(r, d, c.compressor(d))
}

// newDecoderForCompressor returns a decoder for the given compressor value.
func (c *Client) newDecoderForCompressor(r io.Reader, d digest.Digest, compressor rpb.Compressor_Value) (io.ReadCloser, error) {
	switch compressor {
	case rpb.Compressor_IDENTITY:
		return io.NopCloser(r), nil
	case rpb.Compressor_ZSTD:
		buf := probeBufPool.Get().(*bytes.Buffer)
		buf.Reset()
		// Read probeSize+1 so a reader holding exactly probeSize bytes
		// (gRPC delivers EOF only on the read past the last byte) returns
		// io.EOF here and is detected as fitting.
		_, err := io.CopyN(buf, r, probeSize+1)
		var src io.Reader = buf
		switch {
		case errors.Is(err, io.EOF):
			// Entire compressed blob fits in buf. Keeping src as
			// *bytes.Buffer lets zstd use its sync DecodeAll path for
			// small decoded blobs.
		case err == nil:
			// Probe filled, more data remains. MultiReader replays the
			// probed bytes and is not a byter, so zstd uses streaming.
			src = io.MultiReader(buf, r)
		default:
			probeBufPool.Put(buf)
			return nil, err
		}
		if d.SizeBytes < freshDecoderThreshold {
			return c.newPooledBufferedDecoder(src, buf)
		}
		sd, err := zstd.NewReader(src, zstdDecoderOpts...)
		if err != nil {
			probeBufPool.Put(buf)
			return nil, err
		}
		return &discardableDecoder{Decoder: sd, buf: buf}, nil
	case rpb.Compressor_DEFLATE:
		return flate.NewReader(r), nil
	default:
		return nil, fmt.Errorf("unsupported compressor %q", compressor)
	}
}

// newPooledBufferedDecoder takes a pooled decoder, points it at src,
// and pairs it with the probe buf so the buf is recycled on Close.
func (c *Client) newPooledBufferedDecoder(src io.Reader, buf *bytes.Buffer) (io.ReadCloser, error) {
	dec := c.zstdDecoderPool.Get().(*pooledDecoder)
	if err := dec.Reset(src); err != nil {
		probeBufPool.Put(buf)
		return nil, err
	}
	return &bufferedDecoder{pooledDecoder: dec, buf: buf}, nil
}

// discardableDecoder is dropped on Close (not pooled) so the
// window-sized h.b a streaming frame retains, or the syncStream.dstBuf
// a highly-compressible sync DecodeAll retains, can't end up in the
// pool.
type discardableDecoder struct {
	*zstd.Decoder
	// buf is held alive while the MultiReader fed to the decoder still references it.
	buf *bytes.Buffer
}

func (s *discardableDecoder) Close() error {
	if s.buf == nil {
		return nil
	}
	s.Decoder.Close()
	probeBufPool.Put(s.buf)
	s.buf = nil
	return nil
}

type pooledDecoder struct {
	*zstd.Decoder
	pool *sync.Pool
}

func (d *pooledDecoder) Close() error {
	// Drop the io.Reader reference held by the decoder.
	if err := d.Reset(nil); err != nil {
		d.Decoder.Close()
		return err
	}
	d.pool.Put(d)
	return nil
}

// bufferedDecoder pairs a pooled decoder with the probe buf that
// must outlive the decoder's reads.
type bufferedDecoder struct {
	*pooledDecoder
	buf *bytes.Buffer
}

func (b *bufferedDecoder) Close() error {
	if b.buf == nil {
		return nil
	}
	err := b.pooledDecoder.Close()
	probeBufPool.Put(b.buf)
	b.buf = nil
	return err
}

// Get fetches the content of blob from CAS by digest.
// For small blobs, it uses BatchReadBlobs.
// For large blobs, it uses Read method of the ByteStream API
func (c *Client) Get(ctx context.Context, d digest.Digest, name string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("reapi is not configured")
	}
	if d.SizeBytes == 0 {
		return nil, nil
	}

	ctx, span := trace.NewSpan(ctx, "reapi-get")
	defer span.Close(nil)
	span.SetAttr("sizebytes", d.SizeBytes)

	var buf []byte
	var err error
	if d.SizeBytes < c.opt.ByteStreamReadThreshold {
		buf, err = c.getWithBatchReadBlobs(ctx, d, name)
	} else {
		buf, err = c.getWithByteStream(ctx, d, name)
	}
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// GetReader returns an io.ReadCloser to stream the blob content from CAS.
// For small blobs, it fetches the content using BatchReadBlobs and wraps it in a reader.
// For large blobs, it returns the streaming decoder directly from the ByteStream API.
// The caller must close the returned reader.
func (c *Client) GetReader(ctx context.Context, d digest.Digest, name string) (io.ReadCloser, error) {
	if c == nil {
		return nil, fmt.Errorf("reapi is not configured")
	}
	if d.SizeBytes == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if d.SizeBytes < c.opt.ByteStreamReadThreshold {
		buf, err := c.getWithBatchReadBlobs(ctx, d, name)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
	src := digestSource{
		c:     c,
		d:     d,
		fname: name,
	}
	return src.Open(ctx)
}

// getWithBatchReadBlobs fetches the content of blob using BatchReadBlobs rpc of CAS.
func (c *Client) getWithBatchReadBlobs(ctx context.Context, d digest.Digest, name string) ([]byte, error) {
	started := time.Now()
	if log.V(1) {
		clog.Infof(ctx, "getWithBatchReadBlobs %s", d)
	}
	if c.opt.LocalCache != nil && c.opt.LocalCache.HasContent(ctx, d) {
		return c.opt.LocalCache.GetContent(ctx, d, name)
	}
	casClient := rpb.NewContentAddressableStorageClient(c.casConn)
	var resp *rpb.BatchReadBlobsResponse
	err := retry.Do(ctx, func() error {
		var err error
		resp, err = casClient.BatchReadBlobs(ctx, &rpb.BatchReadBlobsRequest{
			InstanceName:          c.opt.Instance,
			Digests:               []*rpb.Digest{d.Proto()},
			AcceptableCompressors: c.acceptableCompressors(d),
			DigestFunction:        c.digestFn.Value(),
		})
		return err
	})
	if err != nil {
		c.m.ReadDone(0, err)
		return nil, fmt.Errorf("failed to read blobs %s for %s in %s: %w", d, name, time.Since(started), err)
	}
	if len(resp.Responses) != 1 {
		c.m.ReadDone(0, err)
		return nil, fmt.Errorf("failed to read blobs %s for %s in %s: responses=%d", d, name, time.Since(started), len(resp.Responses))
	}
	data, err := c.decodeForBatchRead(d, resp.Responses[0].Data, resp.Responses[0].Compressor)
	if err != nil {
		c.m.ReadDone(0, err)
		return nil, fmt.Errorf("failed to decompress blobs %s for %s in %s: %w", d, name, time.Since(started), err)
	}
	c.m.ReadDone(len(data), nil)
	if c.opt.LocalCache != nil {
		err := c.opt.LocalCache.SetContent(ctx, d, name, data)
		if err != nil {
			clog.Warningf(ctx, "failed to write digest %s to local cache: %v", d, err)
		}
	}
	return data, nil
}

// decodeForBatchRead decompresses data from a BatchReadBlobs response.
func (c *Client) decodeForBatchRead(d digest.Digest, data []byte, compressor rpb.Compressor_Value) ([]byte, error) {
	r, err := c.newDecoderForCompressor(bytes.NewReader(data), d, compressor)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	buf := make([]byte, d.SizeBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	if err := expectEOF(r); err != nil {
		return nil, fmt.Errorf("decompressed data exceeds expected size %d", d.SizeBytes)
	}
	return buf, nil
}

// expectEOF verifies that r yields no more data before EOF. After a blob of
// known size has been fully read, the stream must be at EOF; a trailing byte
// means the server sent more than the digest declares, i.e. a blob that does
// not match the requested digest.
func expectEOF(r io.Reader) error {
	switch n, err := io.CopyN(io.Discard, r, 1); {
	case n > 0:
		return errors.New("stream has trailing data past digest size")
	case errors.Is(err, io.EOF):
		return nil
	default:
		return err
	}
}

func (c *Client) bytestreamioOpen(ctx context.Context, d digest.Digest, fname string) (io.ReadCloser, error) {
	if d.SizeBytes == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if d.SizeBytes > c.opt.ChunkedBlobsThreshold {
		c.mu.Lock()
		splitBlobSupport := c.capabilities.GetCacheCapabilities().GetSplitBlobSupport()
		c.mu.Unlock()
		if splitBlobSupport {
			// chunkReader would use LocalCache for chunk data.
			r, err := c.chunkReader(ctx, d, fname)
			if err == nil {
				return r, nil
			}
			clog.Warningf(ctx, "failed chunkReader %s %q: %v", d, fname, err)
		}
		// not supported, or chunkReader failed. fallback.
	}
	if c.opt.LocalCache != nil && c.opt.LocalCache.HasContent(ctx, d) {
		return c.opt.LocalCache.Source(ctx, d, fname).Open(ctx)
	}
	r, err := bytestreamio.Open(ctx, bpb.NewByteStreamClient(c.casDataConn), c.resourceName(d))
	if err != nil {
		return nil, err
	}
	rd, err := c.newDecoder(r, d)
	if err != nil {
		return nil, err
	}
	if c.opt.LocalCache != nil {
		w, err := c.opt.LocalCache.ContentSink(ctx, d, fname)
		if err != nil {
			clog.Warningf(ctx, "failed to write digest %s to local cache: %v", d, err)
		} else if w != nil {
			tr := io.TeeReader(rd, w)
			return &teeCloser{r: tr, rc: rd, wc: w}, nil
		}
	}
	return rd, nil
}

type teeCloser struct {
	r  io.Reader
	rc io.Closer
	wc io.Closer
}

func (r *teeCloser) Read(p []byte) (n int, err error) {
	return r.r.Read(p)
}

func (r *teeCloser) Close() error {
	err := r.rc.Close()
	werr := r.wc.Close()
	if err == nil {
		err = werr
	}
	return err
}

// getWithByteStream fetches the content of blob using the ByteStream API
func (c *Client) getWithByteStream(ctx context.Context, d digest.Digest, name string) ([]byte, error) {
	started := time.Now()
	resourceName := c.resourceName(d)
	if log.V(1) {
		clog.Infof(ctx, "getWithByteStream %s resourceName=%s", d, resourceName)
	}
	buf := make([]byte, d.SizeBytes)
	err := retry.Do(ctx, func() error {
		ctx, cancel := fetch.ContextWithTimeout(ctx, d)
		defer cancel()
		r, err := c.bytestreamioOpen(ctx, d, name)
		if err != nil {
			c.m.ReadDone(0, err)
			return err
		}
		defer r.Close()
		n, err := io.ReadFull(r, buf)
		c.m.ReadDone(n, err)
		if err != nil {
			return err
		}
		if err := expectEOF(r); err != nil {
			clog.Warningf(ctx, "blob %s for %s: %v", d, name, err)
		}
		return nil
	})
	if err != nil {
		return buf, fmt.Errorf("failed to read stream %s for %s in %s: %w", d, name, time.Since(started), err)
	}
	return buf, nil
}

// Missing returns digests of missing blobs.
func (c *Client) Missing(ctx context.Context, blobs []digest.Digest) ([]digest.Digest, error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	blobspb := make([]*rpb.Digest, 0, len(blobs))
	for _, b := range blobs {
		blobspb = append(blobspb, b.Proto())
	}
	cas := rpb.NewContentAddressableStorageClient(c.casConn)

	// Limit each *rpb.FindMissingBlobsRequest under gRPC's default 4MiB message
	// size. Each repeated Digest costs its hex hash plus a small constant of
	// proto framing (sub-message tag/length, hash field tag/length, and the
	// size_bytes field). Derive the batch from the current function's hash length
	// so long hashes (e.g. 128 hex bytes for SHA-512) still stay under the limit;
	// the reserve leaves room for the request's instance_name and digest_function
	// fields.
	const maxRequestBytes = 4 << 20
	const reserveBytes = 4 << 10
	const perDigestOverhead = 16
	maxBlobs := (maxRequestBytes - reserveBytes) / (c.digestFn.HexLen() + perDigestOverhead)

	var ret []digest.Digest
	for len(blobspb) > 0 {
		var remain []*rpb.Digest
		if len(blobspb) > maxBlobs {
			remain = blobspb[maxBlobs:]
			blobspb = blobspb[:maxBlobs]
		}
		var resp *rpb.FindMissingBlobsResponse
		// TODO(b/328332495): grpc should retry by service config?
		err := retry.Do(ctx, func() error {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var err error
			resp, err = cas.FindMissingBlobs(ctx, &rpb.FindMissingBlobsRequest{
				InstanceName:   c.opt.Instance,
				BlobDigests:    blobspb,
				DigestFunction: c.digestFn.Value(),
			})
			c.m.OpsDone(err)
			return err
		})
		if status.Code(err) == codes.DeadlineExceeded {
			// consider blobs are missing and try uploading.
			// uploading may detect they already exist in CAS.
			for _, b := range blobspb {
				ret = append(ret, digest.FromProto(b))
			}
		} else if err != nil {
			return nil, fmt.Errorf("find missing: %w", err)
		} else {
			for _, b := range resp.GetMissingBlobDigests() {
				ret = append(ret, digest.FromProto(b))
			}
		}
		blobspb = remain
	}
	return ret, nil
}

// UploadAll uploads all blobs specified in ds that are still missing in the CAS.
//
// The upload itself is detached from ctx: blobs not yet known to be in
// the CAS are handed as one request to the Client's upload workers,
// which run with the Client's lifetime context (canceled by Close), so
// that other callers waiting on the same blobs are not affected when
// this caller's ctx is canceled. ctx only bounds how long this call
// waits for the result. If ctx is canceled, UploadAll returns the
// cancellation error immediately while the request is still processed
// in the background.
func (c *Client) UploadAll(ctx context.Context, ds *blob.Store) (numUploaded int, err error) {
	return c.uploadAll(ctx, ds, true)
}

// uploadAll implements UploadAll.
//
// When detach is false, the call runs inline on the caller's goroutine
// with ctx and never waits on an upload owned by someone else: blobs
// that are pending in another request are uploaded again by this call
// (CAS uploads are idempotent). This is used for nested uploads issued
// from inside an upload worker (chunkUpload); waiting there on a request
// that is still queued for a worker could deadlock the bounded pool.
func (c *Client) uploadAll(ctx context.Context, ds *blob.Store, detach bool) (numUploaded int, err error) {
	if c.casConn == nil {
		return 0, status.Error(codes.FailedPrecondition, "conn is not configured")
	}

	ctx, span := trace.NewSpan(ctx, "upload-all")
	defer span.Close(nil)
	blobs := ds.List()
	span.SetAttr("blobs", len(blobs))

	// First, partition the "blobs" list into three possible cases.
	newBlobs := make(map[digest.Digest]*uploadOp)
	pendingBlobs := make(map[digest.Digest]*uploadOp)
	skippedBlobs := 0
	for _, d := range blobs {
		uop, loaded := c.knownDigests.LoadOrStore(d, newUploadOp(c, d))
		if loaded {
			switch v := uop.(type) {
			case bool:
				// Case 1: This blob is already present in the CAS, we're done.
				skippedBlobs++
			case *uploadOp:
				if !detach {
					// Inline: upload it ourselves rather than wait.
					// This op is not registered in knownDigests, so
					// its done() leaves the owner's entry untouched.
					newBlobs[d] = newUploadOp(c, d)
					continue
				}
				// Case 2: We need to wait for another upload operation to upload this blob.
				pendingBlobs[d] = v
			default:
				panic(fmt.Sprintf("unknown type %T", v))
			}
			continue
		}
		// Case 3: We need to upload this blob after confirming that it's missing.
		newBlobs[d] = uop.(*uploadOp)
	}
	span.SetAttr("upload", len(newBlobs))
	span.SetAttr("pending", len(pendingBlobs))
	span.SetAttr("skipped", skippedBlobs)

	var durWaitUpload time.Duration
	if len(newBlobs) > 0 {
		t := time.Now()
		if detach {
			req := c.uploader.submit(ds, newBlobs)
			numUploaded, err = req.wait(ctx)
		} else {
			numUploaded, err = c.uploadNewBlobs(ctx, ds, newBlobs, span)
		}
		durWaitUpload = time.Since(t)
		if err != nil {
			return numUploaded, fmt.Errorf("upload %d blobs: %w", len(newBlobs), err)
		}
	}

	// Finally, wait for any blobs that are being uploaded by other upload operations, before we return.
	var durWaitPending time.Duration
	if len(pendingBlobs) > 0 {
		t := time.Now()
		for d, uop := range pendingBlobs {
			if err := uop.wait(ctx); err != nil {
				return numUploaded, fmt.Errorf("wait for digest=%s: %w", d, err)
			}
		}
		durWaitPending = time.Since(t)
	}
	clog.Infof(ctx, "upload all: blobs=%d -> {uploaded=%d, pending=%d, skipped=%d}, wait_upload=%s wait_pending=%s",
		len(blobs), numUploaded, len(pendingBlobs), skippedBlobs,
		durWaitUpload.Round(time.Microsecond),
		durWaitPending.Round(time.Microsecond))

	return numUploaded, nil
}

// uploadNewBlobs checks which blobs in newBlobs are missing in the remote CAS
// and uploads them. On return, all uploadOps in newBlobs are guaranteed to be
// completed.
func (c *Client) uploadNewBlobs(ctx context.Context, ds *blob.Store, newBlobs map[digest.Digest]*uploadOp, span *trace.Span) (numUploaded int, err error) {
	var foundBlobs map[digest.Digest]*uploadOp
	var missingBlobs []digest.Digest
	var durFindMissing, durUpload time.Duration
	defer func() {
		for _, uop := range newBlobs {
			uop.done(cmp.Or(err, errUploadNotFinished))
		}
		clog.Infof(ctx, "upload new blobs: new=%d -> {uploaded=%d, missing=%d, found=%d}, timing: {find_missing=%s, upload=%s}: %v",
			len(newBlobs), numUploaded, len(missingBlobs), len(foundBlobs),
			durFindMissing.Round(time.Microsecond),
			durUpload.Round(time.Microsecond),
			err)
	}()

	// For all "new" blobs, use FindMissingBlobs to ask the remote CAS which of them are
	// really still missing - they might already be present and we just don't know about it yet.
	t := time.Now()
	missingBlobs, err = c.Missing(ctx, slices.Collect(maps.Keys(newBlobs)))
	if err != nil {
		return 0, err
	}
	// All "new" blobs that the remote CAS did *not* report as missing are by inverse confirmed
	// to be present, so we can memoize this in the knownDigests map and notify any other
	// threads waiting for them.
	foundBlobs = maps.Clone(newBlobs)
	for _, d := range missingBlobs {
		delete(foundBlobs, d)
	}
	for _, uop := range foundBlobs {
		uop.done(nil)
	}
	span.SetAttr("missing", len(missingBlobs))
	span.SetAttr("founds", len(foundBlobs))
	durFindMissing = time.Since(t)

	// Let's upload the blobs that we know are still missing.
	if len(missingBlobs) > 0 {
		t := time.Now()
		numUploaded, err = c.upload(ctx, ds, missingBlobs, newBlobs)
		if err != nil {
			return numUploaded, fmt.Errorf("upload: %w", err)
		}
		span.SetAttr("uploaded", numUploaded)
		durUpload = time.Since(t)
	}
	return numUploaded, nil
}

// CheckWritable checks reapi instance writable permission.
func (c *Client) CheckWritable(ctx context.Context) error {
	defer trace.Begin(ctx, "reapi.CheckWritable").End()
	if c.casConn == nil {
		return status.Error(codes.FailedPrecondition, "conn is not configured")
	}
	data := blob.FromBytes(c.digestFn, "empty", nil)
	ds := blob.NewStore()
	ds.Set(data)
	blobs := []digest.Digest{data.Digest()}
	uploads := map[digest.Digest]*uploadOp{
		data.Digest(): newUploadOp(c, data.Digest()),
	}
	_, err := c.upload(ctx, ds, blobs, uploads)
	if err != nil {
		return fmt.Errorf("failed to check writable: %w", err)
	}
	return nil
}

var errBlobNotInReq = errors.New("blob not in request")

type missingBlob struct {
	Digest digest.Digest
	Err    error
}

type missingBlobs struct {
	mu    sync.Mutex
	blobs []missingBlob
}

func (m *missingBlobs) append(mb missingBlob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs = append(m.blobs, mb)
}

func (m *missingBlobs) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.blobs)
}

func (m *missingBlobs) get() []missingBlob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blobs
}

type missingError struct {
	Blobs []missingBlob
}

func (e missingError) Error() string {
	return fmt.Sprintf("missing %d blobs", len(e.Blobs))
}

// upload uploads blobs in digest stores.
func (c *Client) upload(ctx context.Context, ds *blob.Store, blobs []digest.Digest, uploads map[digest.Digest]*uploadOp) (int, error) {
	ctx, span := trace.NewSpan(ctx, "upload")
	defer span.Close(nil)

	byteLimit := int64(defaultBatchUpdateByteLimit)
	c.mu.Lock()
	if max := c.capabilities.GetCacheCapabilities().GetMaxBatchTotalSizeBytes(); max > 0 {
		byteLimit = max
	}
	c.mu.Unlock()

	// Separate small blobs and large blobs because they are going to use different RPCs.
	smalls, larges := separateBlobs(c.opt.Instance, c.digestFn, blobs, byteLimit)
	clog.Infof(ctx, "upload by batch %d out of %d", len(smalls), len(blobs))
	span.SetAttr("small", len(smalls))
	span.SetAttr("large", len(larges))

	// Upload small blobs with BatchUpdateBlobs rpc.
	var missing missingError
	if len(smalls) > 0 {
		missingBlobs, err := c.uploadWithBatchUpdateBlobs(ctx, smalls, uploads, ds, byteLimit)
		if err != nil {
			return 0, fmt.Errorf("upload batch: %w", err)
		}
		missing.Blobs = missingBlobs
	}

	// Upload large blobs with ByteStream API.
	if len(larges) > 0 {
		missingBlobs := c.uploadWithByteStream(ctx, larges, uploads, ds)
		missing.Blobs = append(missing.Blobs, missingBlobs...)
	}

	if len(missing.Blobs) > 0 {
		return len(blobs) - len(missing.Blobs), missing
	}
	return len(blobs), nil
}

// separateBlobs separates blobs to two groups.
// One group is for small blobs that can fit in BatchUpdateBlobsRequest, and the other is for large blobs.
// TODO(b/273884978): simplify and optimize the code.
func separateBlobs(instance string, fn digest.Function, blobs []digest.Digest, byteLimit int64) (smalls, larges []digest.Digest) {
	if len(blobs) == 0 {
		return nil, nil
	}
	sort.Slice(blobs, func(i, j int) bool {
		return blobs[i].SizeBytes < blobs[j].SizeBytes
	})
	maxSizeBytes := min(blobs[len(blobs)-1].SizeBytes, byteLimit)
	// Prepare a dummy request message to calculate the size of the BatchUpdateBlobsRequest accurately.
	dummyReq := &rpb.BatchUpdateBlobsRequest{
		InstanceName:   instance,
		DigestFunction: fn.Value(),
		Requests: []*rpb.BatchUpdateBlobsRequest_Request{
			{Data: make([]byte, 0, maxSizeBytes)},
		},
	}
	i := sort.Search(len(blobs), func(i int) bool {
		if blobs[i].SizeBytes >= byteLimit {
			return true
		}
		// When the BatchUpdateBlobsRequest with the single blob already exceeds the size limit.
		// It can't send the blob via BatchUpdateBlobsRequest.
		dummyReq.Requests[0].Digest = blobs[i].Proto()
		dummyReq.Requests[0].Data = dummyReq.Requests[0].Data[:blobs[i].SizeBytes]
		return int64(proto.Size(dummyReq)) >= byteLimit
	})
	if i < len(blobs) {
		return blobs[:i], blobs[i:]
	}
	return blobs, nil
}

// uploadWithBatchUpdateBlobs uploads blobs using BatchUpdateBlobs RPC.
// The blobs will be bundled into multiple batches that fit in the size limit.
func (c *Client) uploadWithBatchUpdateBlobs(ctx context.Context, digests []digest.Digest, uploads map[digest.Digest]*uploadOp, ds *blob.Store, byteLimit int64) ([]missingBlob, error) {
	blobReqs, missingBlobs := c.blobsToUpload(ctx, digests, ds, byteLimit)

	// Bundle the blobs to multiple batch requests.
	batchReqs := createBatchUpdateBlobsRequests(c.opt.Instance, c.digestFn, blobReqs, byteLimit, batchBlobUploadLimit)
	casClient := rpb.NewContentAddressableStorageClient(c.casConn)

	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(c.uploadConcurrency())
	for batchReq := range batchReqs {
		eg.Go(func() error {
			return c.processBatchUpdateBlobsReq(gctx, casClient, batchReq, uploads, ds, missingBlobs)
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return missingBlobs.get(), nil
}

// processBatchUpdateBlobsReq sends one BatchUpdateBlobs RPC and reconciles the response.
func (c *Client) processBatchUpdateBlobsReq(ctx context.Context, casClient rpb.ContentAddressableStorageClient, batchReq *rpb.BatchUpdateBlobsRequest, uploads map[digest.Digest]*uploadOp, ds *blob.Store, missingBlobs *missingBlobs) error {
	var batchResp *rpb.BatchUpdateBlobsResponse
	checkBlobs := make(map[digest.Digest]struct{}, len(batchReq.Requests))
	for _, req := range batchReq.Requests {
		checkBlobs[digest.FromProto(req.Digest)] = struct{}{}
	}
	// TODO(b/328332495): grpc should retry by service config?
	err := retry.Do(ctx, func() error {
		var err error
		batchResp, err = casClient.BatchUpdateBlobs(ctx, batchReq)
		return err
	})
	if err != nil {
		c.m.WriteDone(0, err)
		return status.Errorf(status.Code(err), "batch update blobs: %v", err)
	}

	for _, res := range batchResp.Responses {
		blob := digest.FromProto(res.Digest)
		delete(checkBlobs, blob)
		data, ok := ds.Get(blob)
		if !ok {
			clog.Warningf(ctx, "Not found %s in store", blob)
			missingBlobs.append(missingBlob{
				Digest: blob,
				Err:    errBlobNotInReq,
			})
			continue
		}
		st := status.FromProto(res.GetStatus())
		if st.Code() != codes.OK {
			clog.Warningf(ctx, "Failed to batch-update %s: %v", data, st)
			err := status.Errorf(st.Code(), "batch update blobs: %v", res.Status)
			missingBlobs.append(missingBlob{
				Digest: blob,
				Err:    err,
			})
			c.m.WriteDone(int(res.Digest.SizeBytes), err)
			uploads[blob].done(err)
			continue
		}
		clog.Infof(ctx, "uploaded in batch: %s", data)
		c.m.WriteDone(int(res.Digest.SizeBytes), nil)
		uploads[blob].done(nil)
	}
	clog.Infof(ctx, "upload by batch %d->%d blobs (noresp:%d) (missing:%d)", len(batchReq.Requests), len(batchResp.Responses), len(checkBlobs), missingBlobs.Size())
	if len(checkBlobs) == 0 {
		return nil
	}
	// check again if digest is missing in batchResp.
	checks := slices.Collect(maps.Keys(checkBlobs))
	clog.Warningf(ctx, "batch no response for %s", checks)
	missings, err := c.Missing(ctx, checks)
	if err != nil {
		clog.Warningf(ctx, "recheck missing %s: %v", checks, err)
		for _, d := range checks {
			uploads[d].done(err)
			missingBlobs.append(missingBlob{
				Digest: d,
				Err:    err,
			})
		}
		return nil
	}
	for _, d := range missings {
		clog.Warningf(ctx, "recheck missing %s: not uploaded", d)
		uploads[d].done(errUploadNoResponse)
		missingBlobs.append(missingBlob{
			Digest: d,
			Err:    errUploadNoResponse,
		})
		delete(checkBlobs, d)
	}
	// checkBlobs has blobs that are not reported in BatchUpdateBlobsResponse,
	// but not reported as missing by FindMissing, so we can believe them
	// exists in CAS.
	for d := range checkBlobs {
		clog.Warningf(ctx, "recheck missing %s: exists", d)
		uploads[d].done(nil)
	}
	return nil
}

// blobsToUpload returns a list of blobs to upload by looking up the digest store.
func (c *Client) blobsToUpload(ctx context.Context, blobs []digest.Digest, ds *blob.Store, byteLimit int64) (iter.Seq[*rpb.BatchUpdateBlobsRequest_Request], *missingBlobs) {
	var missings missingBlobs
	ch := make(chan *rpb.BatchUpdateBlobsRequest_Request)

	go func() {
		defer close(ch)
		// allocate for at most 2 BatchUpdateBlobsRequest messages.
		sema := xsemaphore.NewWeighted(byteLimit * 2)
		var wg sync.WaitGroup
		for _, blob := range blobs {
			err := sema.Acquire(ctx, blob.SizeBytes)
			if err != nil {
				missings.append(missingBlob{
					Digest: blob,
					Err:    err,
				})
				clog.Warningf(ctx, "failed to acquire %d: %v", blob.SizeBytes, err)
				continue
			}
			wg.Go(func() {
				defer sema.Release(blob.SizeBytes)
				data, ok := ds.Get(blob)
				if !ok {
					missings.append(missingBlob{
						Digest: blob,
						Err:    errBlobNotInReq,
					})
					clog.Warningf(ctx, "missing %s to upload: %v", blob, errBlobNotInReq)
					return
				}
				var req *rpb.BatchUpdateBlobsRequest_Request
				err := FileSemaphore.Do(ctx, func(ctx context.Context) error {
					var err error
					req, err = c.encodeForBatchUpload(ctx, data)
					return err
				})
				if err != nil {
					missings.append(missingBlob{
						Digest: blob,
						Err:    err,
					})
					clog.Warningf(ctx, "read %s to upload: %v", blob, err)
					return
				}
				ch <- req
			})
		}
		wg.Wait()
	}()
	return func(yield func(*rpb.BatchUpdateBlobsRequest_Request) bool) {
		for req := range ch {
			if !yield(req) {
				return
			}
		}
	}, &missings
}

// encodeForBatchUpload reads blob data and optionally compresses it for batch upload.
func (c *Client) encodeForBatchUpload(ctx context.Context, data blob.Data) (*rpb.BatchUpdateBlobsRequest_Request, error) {
	d := data.Digest()
	compressor := c.compressorForBatchUpdate(d)
	b, err := blob.DataToBytes(ctx, data)
	if err != nil {
		return nil, err
	}
	if compressor == rpb.Compressor_IDENTITY {
		return &rpb.BatchUpdateBlobsRequest_Request{
			Digest: d.Proto(),
			Data:   b,
		}, nil
	}
	var buf bytes.Buffer
	w, err := newEncoderForCompressor(&buf, compressor)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &rpb.BatchUpdateBlobsRequest_Request{
		Digest:     d.Proto(),
		Data:       buf.Bytes(),
		Compressor: compressor,
	}, nil
}

// createBatchUpdateBlobsRequests bundles blobs into multiple batch requests.
func createBatchUpdateBlobsRequests(instance string, fn digest.Function, blobReqs iter.Seq[*rpb.BatchUpdateBlobsRequest_Request], byteLimit int64, numLimit int) iter.Seq[*rpb.BatchUpdateBlobsRequest] {
	return func(yield func(*rpb.BatchUpdateBlobsRequest) bool) {
		// Initial batch request size without blobs: the instance name plus
		// the digest_function field, which every emitted request carries.
		batchReqNoReqsSize := int64(proto.Size(&rpb.BatchUpdateBlobsRequest{
			InstanceName:   instance,
			DigestFunction: fn.Value(),
		}))
		size := batchReqNoReqsSize
		// totalSizeBytes tracks the sum of uncompressed Digest.SizeBytes
		// in the current batch. Some backends interpret
		// MaxBatchTotalSizeBytes as this sum rather than the proto size,
		// so both constraints must be satisfied.
		var totalSizeBytes int64
		var reqs []*rpb.BatchUpdateBlobsRequest_Request
		for req := range blobReqs {
			reqs = append(reqs, req)
			nextSize := size + int64(proto.Size(&rpb.BatchUpdateBlobsRequest{Requests: reqs[len(reqs)-1:]}))
			nextTotalSizeBytes := totalSizeBytes + req.Digest.SizeBytes
			switch {
			case byteLimit > 0 && (nextSize > byteLimit || nextTotalSizeBytes > byteLimit):
				// When the batch request exceeds the size limit, it starts creating a new batch request.
				if !yield(&rpb.BatchUpdateBlobsRequest{
					InstanceName:   instance,
					Requests:       reqs[:len(reqs)-1],
					DigestFunction: fn.Value(),
				}) {
					return
				}
				size = batchReqNoReqsSize + nextSize - size
				totalSizeBytes = req.Digest.SizeBytes
				reqs = []*rpb.BatchUpdateBlobsRequest_Request{req}

			case len(reqs) == numLimit:
				// When the batch request exceeds the number of blobs. it start creating a new batch request.
				if !yield(&rpb.BatchUpdateBlobsRequest{
					InstanceName:   instance,
					Requests:       reqs,
					DigestFunction: fn.Value(),
				}) {
					return
				}
				size = batchReqNoReqsSize
				totalSizeBytes = 0
				reqs = nil
			default:
				size = nextSize
				totalSizeBytes = nextTotalSizeBytes
			}
		}
		if len(reqs) > 0 {
			yield(&rpb.BatchUpdateBlobsRequest{
				InstanceName:   instance,
				Requests:       reqs,
				DigestFunction: fn.Value(),
			})
		}
	}
}

func (c *Client) uploadWithByteStream(ctx context.Context, digests []digest.Digest, uploads map[digest.Digest]*uploadOp, ds *blob.Store) []missingBlob {
	clog.Infof(ctx, "upload by streaming %d", len(digests))

	var (
		mu      sync.Mutex
		missing []missingBlob
	)
	addMissing := func(mb missingBlob) {
		mu.Lock()
		missing = append(missing, mb)
		mu.Unlock()
	}

	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(c.uploadConcurrency())
	for _, d := range digests {
		eg.Go(func() error {
			c.streamOneBlob(gctx, d, uploads, ds, addMissing)
			return nil
		})
	}
	_ = eg.Wait()
	clog.Infof(ctx, "uploaded by streaming %d blobs (missing:%d)", len(digests), len(missing))
	return missing
}

func (c *Client) bytestreamioUpload(ctx context.Context, d digest.Digest, fname string, rd io.Reader) error {
	if d.SizeBytes > c.opt.ChunkedBlobsThreshold {
		c.mu.Lock()
		spliceBlobSupport := c.capabilities.GetCacheCapabilities().GetSpliceBlobSupport()
		c.mu.Unlock()
		if spliceBlobSupport {
			err := c.chunkUpload(ctx, d, fname, rd)
			if err == nil {
				return nil
			}
			clog.Warningf(ctx, "failed chunkUpload %s %q: %v", d, fname, err)
		}
		// not supported, or chunkUpload failed, fallback.

	}
	bsClient := bpb.NewByteStreamClient(c.casDataConn)
	wr, err := bytestreamio.Create(ctx, bsClient, c.uploadResourceName(d), fname)
	if err != nil {
		return err
	}
	cwr, err := c.newEncoder(wr, d)
	if err != nil {
		wr.Close()
		return err
	}
	_, err = io.Copy(cwr, rd)
	if err != nil {
		cwr.Close()
		wr.Close()
		return err
	}
	err = cwr.Close()
	if err != nil {
		wr.Close()
		return err
	}
	return wr.Close()
}

// streamOneBlob uploads one blob via ByteStream; errors go to addMissing, not propagated.
func (c *Client) streamOneBlob(ctx context.Context, d digest.Digest, uploads map[digest.Digest]*uploadOp, ds *blob.Store, addMissing func(missingBlob)) {
	started := time.Now()
	data, ok := ds.Get(d)
	if !ok {
		clog.Warningf(ctx, "Not found %s in store", d)
		addMissing(missingBlob{Digest: d, Err: errBlobNotInReq})
		return
	}
	err := retry.Do(ctx, func() error {
		ctx, cancel := fetch.ContextWithTimeout(ctx, d)
		defer cancel()
		rd, err := data.Open(ctx)
		if err != nil {
			return err
		}
		defer rd.Close()
		if log.V(1) {
			clog.Infof(ctx, "put %s", c.uploadResourceName(d))
		}
		err = c.bytestreamioUpload(ctx, d, data.String(), rd)
		if err != nil {
			// Some REAPI backends may return non-standard committed size,
			// or error in CloseAndRecv. Check via FindMissingBlobs whether
			// the blob is already uploaded or not.
			ds, merr := c.Missing(ctx, []digest.Digest{d})
			if merr == nil && len(ds) == 0 {
				clog.Infof(ctx, "bytestreamioUpload: %v -> %v exists in CAS", err, d)
				return nil
			}
			clog.Warningf(ctx, "bytestreamioUpload: %v -> missing %v, %v", err, ds, merr)
			return status.Errorf(codes.Internal, "%v", err)
		}
		return nil
	})
	c.m.WriteDone(int(d.SizeBytes), err)
	uploads[d].done(err)
	if err != nil {
		clog.Warningf(ctx, "Failed to stream %s in %s: %v", data, time.Since(started), err)
		addMissing(missingBlob{Digest: d, Err: err})
		return
	}
	clog.Infof(ctx, "uploaded streaming %s in %s", data, time.Since(started))
}

// resourceName constructs a resource name for uploading the blob identified by the digest.
// For uncompressed blob. the format is
//
// `{instance_name}/uploads/{uuid}/blobs/{digest_function/}{hash}/{size}`
//
// # For compressed blob, the format is
//
// `{instance_name}/uploads/{uuid}/compressed-blobs/{compressor}/{digest_function/}{uncompressed_hash}/{uncompressed_size}`
//
// The digest_function segment is omitted for sha256 (per spec); path.Join drops
// empty segments.
//
// See also the API document.
// https://github.com/bazelbuild/remote-apis/blob/64cc5e9e422c93e1d7f0545a146fd84fcc0e8b47/build/bazel/remote/execution/v2/remote_execution.proto#L211-L239
func (c *Client) uploadResourceName(d digest.Digest) string {
	if compressor := c.compressor(d); compressor != rpb.Compressor_IDENTITY {
		return path.Join(c.opt.Instance, "uploads", uuid.New().String(),
			"compressed-blobs",
			strings.ToLower(compressor.String()),
			c.digestFunctionSegment(),
			d.Hash,
			strconv.FormatInt(d.SizeBytes, 10))
	}
	return path.Join(c.opt.Instance, "uploads", uuid.New().String(), "blobs", c.digestFunctionSegment(), d.Hash, strconv.FormatInt(d.SizeBytes, 10))
}

// FileURI returns bytestream URI for digest.
func (c *Client) FileURI(d digest.Digest) string {
	// compressed-blobs is not supported?
	return fmt.Sprintf("bytestream://%s/%s", c.opt.Address, path.Join(c.opt.Instance, "blobs", c.digestFunctionSegment(), d.Hash, strconv.FormatInt(d.SizeBytes, 10)))
}

// newEncoder returns an encoder to compress blob.
// For uncompressed blob, it returns a nop closer.
func (c *Client) newEncoder(w io.Writer, d digest.Digest) (io.WriteCloser, error) {
	return newEncoderForCompressor(w, c.compressor(d))
}

// newEncoderForCompressor returns an encoder for the given compressor value.
func newEncoderForCompressor(w io.Writer, compressor rpb.Compressor_Value) (io.WriteCloser, error) {
	switch compressor {
	case rpb.Compressor_IDENTITY:
		return nopWriteCloser{w}, nil
	case rpb.Compressor_ZSTD:
		return zstd.NewWriter(w)
	case rpb.Compressor_DEFLATE:
		return flate.NewWriter(w, flate.DefaultCompression)
	default:
		return nil, fmt.Errorf("unsupported compressor %q", compressor)
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }
