// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	bpb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/bytestreamio"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/retry"
	"go.chromium.org/build/siso/runtimex"
	"go.chromium.org/build/siso/sync/semaphore"
)

var (
	// FileSemaphore limits concurrent file access to create BatchUpdateBlobgs to protect from runtime thread exhaustion.
	FileSemaphore = semaphore.New("reapi-cas-file", runtimex.NumCPU())
)

const (
	// bytestreamReadThreshold is the threshold that decides whether to use BatchReadBlobs or ByteStream API.
	bytestreamReadThreshold = 2 * 1024 * 1024

	// defaultBatchUpdateByteLimit is bytes limit for cas BatchUpdateBlobs.
	defaultBatchUpdateByteLimit = 4 * 1024 * 1024

	// batchBlobUploadLimit is max number of blobs in BatchUpdateBlobs.
	batchBlobUploadLimit = 1000
)

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
	ch  chan struct{}
	err error
}

func newUploadOp() *uploadOp {
	return &uploadOp{
		ch:  make(chan struct{}),
		err: errUploadNotFinished,
	}
}

func (u *uploadOp) done(err error) {
	u.err = err
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

var errUploadNotFinished = errors.New("upload not finished")
var errUploadNoResponse = errors.New("upload no response")

func (c *Client) useCompressedBlob(d digest.Digest) bool {
	if c.opt.compressor == rpb.Compressor_IDENTITY {
		return false
	}
	return d.SizeBytes >= c.opt.CompressedBlob
}

// getCompressor returns a compressor for ByteStream Read/Write APIs.
func (c *Client) getCompressor() rpb.Compressor_Value {
	return c.opt.compressor
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
	if c.useCompressedBlob(d) {
		return path.Join(c.opt.Instance, "compressed-blobs",
			strings.ToLower(c.getCompressor().String()),
			d.Hash, strconv.FormatInt(d.SizeBytes, 10))
	}
	return path.Join(c.opt.Instance, "blobs", d.Hash, strconv.FormatInt(d.SizeBytes, 10))
}

// newDecoder returns a decoder to uncompress blob.
// For uncompressed blob, it returns a nop closer.
func (c *Client) newDecoder(r io.Reader, d digest.Digest) (io.ReadCloser, error) {
	if c.useCompressedBlob(d) {
		switch comp := c.getCompressor(); comp {
		case rpb.Compressor_ZSTD:
			rd, err := zstd.NewReader(r)
			return rd.IOReadCloser(), err
		case rpb.Compressor_DEFLATE:
			return flate.NewReader(r), nil
		default:
			return nil, fmt.Errorf("unsupported compressor %q", comp)
		}
	}
	return io.NopCloser(r), nil
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

	if d.SizeBytes < bytestreamReadThreshold {
		return c.getWithBatchReadBlobs(ctx, d, name)
	}
	return c.getWithByteStream(ctx, d, name)
}

// getWithBatchReadBlobs fetches the content of blob using BatchReadBlobs rpc of CAS.
func (c *Client) getWithBatchReadBlobs(ctx context.Context, d digest.Digest, name string) ([]byte, error) {
	started := time.Now()
	casClient := rpb.NewContentAddressableStorageClient(c.casConn)
	var resp *rpb.BatchReadBlobsResponse
	err := retry.Do(ctx, func() error {
		var err error
		resp, err = casClient.BatchReadBlobs(ctx, &rpb.BatchReadBlobsRequest{
			InstanceName: c.opt.Instance,
			Digests:      []*rpb.Digest{d.Proto()},
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
	c.m.ReadDone(len(resp.Responses[0].Data), err)
	if int64(len(resp.Responses[0].Data)) != d.SizeBytes {
		return nil, fmt.Errorf("failed to read blobs %s for %s in %s: size mismatch got=%d", d, name, time.Since(started), len(resp.Responses[0].Data))
	}
	return resp.Responses[0].Data, nil
}

// getWithByteStream fetches the content of blob using the ByteStream API
func (c *Client) getWithByteStream(ctx context.Context, d digest.Digest, name string) ([]byte, error) {
	started := time.Now()
	resourceName := c.resourceName(d)
	if log.V(1) {
		clog.Infof(ctx, "get %s", resourceName)
	}
	var buf []byte
	err := retry.Do(ctx, func() error {
		ctx, cancel := digest.ContextWithTimeout(ctx, d)
		defer cancel()
		r, err := bytestreamio.Open(ctx, bpb.NewByteStreamClient(c.casConn), resourceName)
		if err != nil {
			c.m.ReadDone(0, err)
			return err
		}
		rd, err := c.newDecoder(r, d)
		if err != nil {
			c.m.ReadDone(0, err)
			return err
		}
		defer rd.Close()
		buf = make([]byte, d.SizeBytes)
		n, err := io.ReadFull(rd, buf)
		c.m.ReadDone(n, err)
		if err != nil {
			return err
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
	blobspb := make([]*rpb.Digest, 0, len(blobs))
	for _, b := range blobs {
		blobspb = append(blobspb, b.Proto())
	}
	cas := rpb.NewContentAddressableStorageClient(c.casConn)

	var ret []digest.Digest
	for len(blobspb) > 0 {
		var remain []*rpb.Digest
		// limit *rpb.FindMissingBlobsRequest size under 4MB.
		// each digest is sha256 64 bytes + size 4 bytes.
		// 48k is sufficiently large that would never exceeds 4MB.
		const maxBlobs = 48 * 1024
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
				InstanceName: c.opt.Instance,
				BlobDigests:  blobspb,
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
func (c *Client) UploadAll(ctx context.Context, ds *digest.Store) (numUploaded int, err error) {
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
		uop, loaded := c.knownDigests.LoadOrStore(d, newUploadOp())
		if loaded {
			switch v := uop.(type) {
			case bool:
				// Case 1: This blob is already present in the CAS, we're done.
				skippedBlobs++
			case *uploadOp:
				// Case 2: We need to wait for another thread to upload this blob.
				pendingBlobs[d] = v
			default:
				panic(fmt.Sprintf("unknown type %T", v))
			}
			continue
		}
		// Case 3: We need to upload this blob after confirming that it's missing.
		newBlobs[d] = uop.(*uploadOp)
	}
	var foundBlobs map[digest.Digest]bool
	var durFindMissing time.Duration
	var durUpload time.Duration
	var durWaitPending time.Duration
	defer func() {
		for d, uop := range newBlobs {
			if uop.err == nil {
				// uop.done(nil) was called
				c.knownDigests.CompareAndSwap(d, uop, true)
				continue
			}
			var s string
			if data, ok := ds.Get(d); ok {
				s = data.String()
			} else {
				s = d.String()
			}
			if errors.Is(uop.err, errUploadNotFinished) {
				// uop.done(err) is not called
				if err != nil {
					uop.err = err
				}
				clog.Warningf(ctx, "upload %s not finished: %v", s, err)
				close(uop.ch)
			} else {
				// uop.done(err) was called with non-nil err.
				clog.Warningf(ctx, "upload %s failed: %v", s, uop.err)
			}
			// forget this digest, so next will try to upload again.
			c.knownDigests.CompareAndDelete(d, uop)
		}
		clog.Infof(ctx, "upload all: blobs=%d -> {uploaded=%d, found=%d, pending=%d, skipped=%d}, timing: {find_missing=%s, upload=%s, wait_pending=%s}: %v",
			len(blobs), len(newBlobs), len(foundBlobs), len(pendingBlobs), skippedBlobs,
			durFindMissing.Round(time.Microsecond),
			durUpload.Round(time.Microsecond),
			durWaitPending.Round(time.Microsecond),
			err)
	}()

	span.SetAttr("upload", len(newBlobs))
	span.SetAttr("pending", len(pendingBlobs))
	span.SetAttr("skipped", skippedBlobs)

	// For all "new" blobs, use FindMissingBlobs to ask the remote CAS which of them are
	// really still missing - they might already be present and we just don't know about it yet.
	var missingBlobs []digest.Digest
	if len(newBlobs) > 0 {
		t := time.Now()
		newBlobDigests := make([]digest.Digest, 0, len(newBlobs))
		foundBlobs = make(map[digest.Digest]bool, len(newBlobs))
		for d := range newBlobs {
			newBlobDigests = append(newBlobDigests, d)
			foundBlobs[d] = true
		}
		missingBlobs, err = c.Missing(ctx, newBlobDigests)
		if err != nil {
			for _, v := range newBlobs {
				v.done(err)
			}
			return 0, err
		}
		// All "new" blobs that the remote CAS did *not* report as missing are by inverse confirmed
		// to be present, so we can memoize this in the knownDigests map and notify any other
		// threads waiting for them.
		for _, d := range missingBlobs {
			delete(foundBlobs, d)
		}
		for d := range foundBlobs {
			c.knownDigests.CompareAndSwap(d, newBlobs[d], true)
			newBlobs[d].done(nil)
			delete(newBlobs, d)
		}
		span.SetAttr("missing", len(missingBlobs))
		span.SetAttr("founds", len(foundBlobs))
		durFindMissing = time.Since(t)
	}

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

	// Finally, wait for any blobs that are being uploaded by other threads, before we return.
	if len(pendingBlobs) > 0 {
		t := time.Now()
		for d, uop := range pendingBlobs {
			if err := uop.wait(ctx); err != nil {
				return numUploaded, fmt.Errorf("wait for digest=%s: %w", d, err)
			}
		}
		durWaitPending = time.Since(t)
	}

	return numUploaded, err
}

var (
	errBlobNotInReq = errors.New("blob not in request")
)

type missingBlob struct {
	Digest digest.Digest
	Err    error
}

type missingError struct {
	Blobs []missingBlob
}

func (e missingError) Error() string {
	return fmt.Sprintf("missing %d blobs", len(e.Blobs))
}

// upload uploads blobs in digest stores.
func (c *Client) upload(ctx context.Context, ds *digest.Store, blobs []digest.Digest, uploads map[digest.Digest]*uploadOp) (int, error) {
	ctx, span := trace.NewSpan(ctx, "upload")
	defer span.Close(nil)

	byteLimit := int64(defaultBatchUpdateByteLimit)
	if max := c.capabilities.GetCacheCapabilities().GetMaxBatchTotalSizeBytes(); max > 0 {
		byteLimit = max
	}

	// Separate small blobs and large blobs because they are going to use different RPCs.
	smalls, larges := separateBlobs(c.opt.Instance, blobs, byteLimit)
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
func separateBlobs(instance string, blobs []digest.Digest, byteLimit int64) (smalls, larges []digest.Digest) {
	if len(blobs) == 0 {
		return nil, nil
	}
	sort.Slice(blobs, func(i, j int) bool {
		return blobs[i].SizeBytes < blobs[j].SizeBytes
	})
	maxSizeBytes := min(blobs[len(blobs)-1].SizeBytes, byteLimit)
	// Prepare a dummy request message to calculate the size of the BatchUpdateBlobsRequest accurately.
	dummyReq := &rpb.BatchUpdateBlobsRequest{
		InstanceName: instance,
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
func (c *Client) uploadWithBatchUpdateBlobs(ctx context.Context, digests []digest.Digest, uploads map[digest.Digest]*uploadOp, ds *digest.Store, byteLimit int64) ([]missingBlob, error) {
	blobReqs, missingBlobs := blobsToUpload(ctx, digests, ds)

	// Bundle the blobs to multiple batch requests.
	batchReqs := createBatchUpdateBlobsRequests(c.opt.Instance, blobReqs, byteLimit)
	casClient := rpb.NewContentAddressableStorageClient(c.casConn)

	// TODO(b/273884978): It may be worth trying to send the batch requests in parallel.
	for _, batchReq := range batchReqs {
		var batchResp *rpb.BatchUpdateBlobsResponse
		checkBlobs := make(map[digest.Digest]bool)
		for _, req := range batchReq.Requests {
			checkBlobs[digest.FromProto(req.Digest)] = true
		}
		// TODO(b/328332495): grpc should retry by service config?
		err := retry.Do(ctx, func() error {
			var err error
			batchResp, err = casClient.BatchUpdateBlobs(ctx, batchReq)
			return err
		})
		if err != nil {
			c.m.WriteDone(0, err)
			return nil, status.Errorf(status.Code(err), "batch update blobs: %v", err)
		}

		for _, res := range batchResp.Responses {
			blob := digest.FromProto(res.Digest)
			delete(checkBlobs, blob)
			data, ok := ds.Get(blob)
			if !ok {
				clog.Warningf(ctx, "Not found %s in store", blob)
				missingBlobs = append(missingBlobs, missingBlob{
					Digest: blob,
					Err:    errBlobNotInReq,
				})
				continue
			}
			st := status.FromProto(res.GetStatus())
			if st.Code() != codes.OK {
				clog.Warningf(ctx, "Failed to batch-update %s: %v", data, st)
				err := status.Errorf(st.Code(), "batch update blobs: %v", res.Status)
				missingBlobs = append(missingBlobs, missingBlob{
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
		clog.Infof(ctx, "upload by batch %d->%d blobs (noresp:%d) (missing:%d)", len(batchReq.Requests), len(batchResp.Responses), len(checkBlobs), len(missingBlobs))
		if len(checkBlobs) > 0 {
			// check again if digest is missing in batchResp.
			checks := slices.Collect(maps.Keys(checkBlobs))
			clog.Warningf(ctx, "batch no response for %s", checks)
			missings, err := c.Missing(ctx, checks)
			if err != nil {
				clog.Warningf(ctx, "recheck missing %s: %v", checks, err)
				for _, d := range checks {
					uploads[d].done(err)
					missingBlobs = append(missingBlobs, missingBlob{
						Digest: d,
						Err:    err,
					})
				}
			} else {
				for _, d := range missings {
					clog.Warningf(ctx, "recheck missing %s: not uploaded", d)
					uploads[d].done(errUploadNoResponse)
					missingBlobs = append(missingBlobs, missingBlob{
						Digest: d,
						Err:    errUploadNoResponse,
					})
					delete(checkBlobs, d)
				}
				// checkBlobs has blobs that are not reported
				// in BatchUpdateBlobsResponse, but not reported
				// as missing by FindMissing, so we can believe
				// them exists in CAS.
				for d := range checkBlobs {
					clog.Warningf(ctx, "recheck missing %s: exists", d)
					uploads[d].done(nil)
				}
			}
		}
	}
	return missingBlobs, nil
}

// blobsToUpload returns a list of blobs to upload by looking up the digest store.
func blobsToUpload(ctx context.Context, blobs []digest.Digest, ds *digest.Store) ([]*rpb.BatchUpdateBlobsRequest_Request, []missingBlob) {
	var wg sync.WaitGroup
	type res struct {
		err error
		req *rpb.BatchUpdateBlobsRequest_Request
	}
	results := make([]res, len(blobs))
	for i := range blobs {
		wg.Add(1)
		go func(blob digest.Digest, result *res) {
			defer wg.Done()
			data, ok := ds.Get(blob)
			if !ok {
				result.err = errBlobNotInReq
				clog.Warningf(ctx, "missing %s to upload: %v", blob, result.err)
				return
			}
			var b []byte
			err := FileSemaphore.Do(ctx, func(ctx context.Context) error {
				var err error
				b, err = readAll(ctx, data)
				return err
			})
			if err != nil {
				result.err = err
				clog.Warningf(ctx, "read %s to upload: %v", blob, err)
				return
			}
			result.req = &rpb.BatchUpdateBlobsRequest_Request{
				Digest: data.Digest().Proto(),
				Data:   b,
			}
		}(blobs[i], &results[i])
	}
	wg.Wait()

	var reqs []*rpb.BatchUpdateBlobsRequest_Request
	var missings []missingBlob
	for i, result := range results {
		blob := blobs[i]
		switch {
		case result.err != nil:
			missings = append(missings, missingBlob{
				Digest: blob,
				Err:    result.err,
			})
		case result.req != nil:
			reqs = append(reqs, result.req)
			continue
		default:
			clog.Errorf(ctx, "lookup of blobs[%d]=%v no error nor req", i, blob)
		}
	}
	return reqs, missings
}

// createBatchUpdateBlobsRequests bundles blobs into multiple batch requests.
func createBatchUpdateBlobsRequests(instance string, blobReqs []*rpb.BatchUpdateBlobsRequest_Request, byteLimit int64) []*rpb.BatchUpdateBlobsRequest {
	var batchReqs []*rpb.BatchUpdateBlobsRequest

	// Initial batch request size without blobs.
	batchReqNoReqsSize := int64(proto.Size(&rpb.BatchUpdateBlobsRequest{InstanceName: instance}))
	size := batchReqNoReqsSize

	lastOffset := 0
	for i := range blobReqs {
		size += int64(proto.Size(&rpb.BatchUpdateBlobsRequest{Requests: blobReqs[i : i+1]}))
		switch {
		case i == len(blobReqs)-1:
			fallthrough
		case i+1 == lastOffset+batchBlobUploadLimit:
			fallthrough
		case byteLimit > 0 && size+int64(proto.Size(&rpb.BatchUpdateBlobsRequest{Requests: blobReqs[i+1 : i+2]})) > byteLimit:
			// When the batch request exceeds the size limit, it starts creating a new batch request.
			batchReqs = append(batchReqs, &rpb.BatchUpdateBlobsRequest{
				InstanceName: instance,
				Requests:     blobReqs[lastOffset : i+1],
			})
			size = batchReqNoReqsSize
			lastOffset = i + 1
		}
	}
	return batchReqs
}

func readAll(ctx context.Context, data digest.Data) ([]byte, error) {
	var buf []byte
	err := retry.Do(ctx, func() error {
		r, err := data.Open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		buf, err = io.ReadAll(r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func (c *Client) uploadWithByteStream(ctx context.Context, digests []digest.Digest, uploads map[digest.Digest]*uploadOp, ds *digest.Store) []missingBlob {
	clog.Infof(ctx, "upload by streaming %d", len(digests))

	var missingBlobs []missingBlob
	bsClient := bpb.NewByteStreamClient(c.casConn)
	for _, d := range digests {
		started := time.Now()
		data, ok := ds.Get(d)
		if !ok {
			clog.Warningf(ctx, "Not found %s in store", d)
			missingBlobs = append(missingBlobs, missingBlob{
				Digest: d,
				Err:    errBlobNotInReq,
			})
			continue
		}
		err := retry.Do(ctx, func() error {
			ctx, cancel := digest.ContextWithTimeout(ctx, d)
			defer cancel()
			rd, err := data.Open(ctx)
			if err != nil {
				return err
			}
			defer rd.Close()
			resourceName := c.uploadResourceName(d)
			if log.V(1) {
				clog.Infof(ctx, "put %s", resourceName)
			}
			wr, err := bytestreamio.Create(ctx, bsClient, c.uploadResourceName(d), data.String())
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
			var cserr bytestreamio.BadCommittedSizeError
			err = wr.Close()
			if errors.As(err, &cserr) {
				// Some REAPI backends may return non-standard
				// committed size.
				// Check it by FindMissingBlobs API to see if
				// the blob is already uploaded or not.
				ds, merr := c.Missing(ctx, []digest.Digest{d})
				if merr == nil && len(ds) == 0 {
					clog.Infof(ctx, "bytestreamio.Create: %v -> %v exists in CAS", err, d)
					err = nil
				} else {
					clog.Warningf(ctx, "bytestreamio.Create: %v -> missing %v, %v", err, ds, merr)
					err = status.Errorf(codes.Internal, "%v", err)
				}
			}
			return err
		})
		c.m.WriteDone(int(d.SizeBytes), err)
		uploads[d].done(err)
		if err != nil {
			clog.Warningf(ctx, "Failed to stream %s in %s: %v", data, time.Since(started), err)
			missingBlobs = append(missingBlobs, missingBlob{
				Digest: d,
				Err:    err,
			})
			continue
		}
		clog.Infof(ctx, "uploaded streaming %s in %s err=%v", data, time.Since(started), err)
	}
	clog.Infof(ctx, "uploaded by streaming %d blobs (missing:%d)", len(digests), len(missingBlobs))

	return missingBlobs
}

// resourceName constructs a resource name for uploading the blob identified by the digest.
// For uncompressed blob. the format is
//
// `{instance_name}/uploads/{uuid}/blobs/{hash}/{size}`
//
// # For compressed blob, the format is
//
// `{instance_name}/uploads/{uuid}/compressed-blobs/{compressor}/{uncompressed_hash}/{uncompressed_size}`
//
// See also the API document.
// https://github.com/bazelbuild/remote-apis/blob/64cc5e9e422c93e1d7f0545a146fd84fcc0e8b47/build/bazel/remote/execution/v2/remote_execution.proto#L211-L239
func (c *Client) uploadResourceName(d digest.Digest) string {
	if c.useCompressedBlob(d) {
		return path.Join(c.opt.Instance, "uploads", uuid.New().String(),
			"compressed-blobs",
			strings.ToLower(c.getCompressor().String()),
			d.Hash,
			strconv.FormatInt(d.SizeBytes, 10))
	}
	return path.Join(c.opt.Instance, "uploads", uuid.New().String(), "blobs", d.Hash, strconv.FormatInt(d.SizeBytes, 10))
}

// FileURI returns bytestream URI for digest.
func (c *Client) FileURI(d digest.Digest) string {
	// compressed-blobs is not supported?
	return fmt.Sprintf("bytestream://%s/%s", c.opt.Address, path.Join(c.opt.Instance, "blobs", d.Hash, strconv.FormatInt(d.SizeBytes, 10)))
}

// newEncoder returns an encoder to compress blob.
// For uncompressed blob, it returns a nop closer.
func (c *Client) newEncoder(w io.Writer, d digest.Digest) (io.WriteCloser, error) {
	if c.useCompressedBlob(d) {
		switch comp := c.getCompressor(); comp {
		case rpb.Compressor_ZSTD:
			return zstd.NewWriter(w)
		case rpb.Compressor_DEFLATE:
			return flate.NewWriter(w, flate.DefaultCompression)
		default:
			return nil, fmt.Errorf("unsupported compressor %q", comp)
		}
	}
	return nopWriteCloser{w}, nil
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }
