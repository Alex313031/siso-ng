// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	bpb "google.golang.org/genproto/googleapis/bytestream"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/auth/cred"
)

type fakeChunkCAS struct {
	rpb.UnimplementedContentAddressableStorageServer
	bpb.UnimplementedByteStreamServer

	mu                   sync.Mutex
	splitBlobMap         map[digest.Digest][]*rpb.Digest
	blobs                map[digest.Digest][]byte
	batchReadReqs        []*rpb.BatchReadBlobsRequest
	batchUpdateReqs      []*rpb.BatchUpdateBlobsRequest
	splitReqs            []*rpb.SplitBlobRequest
	spliceReqs           []*rpb.SpliceBlobRequest
	spliceBlobErr        error
	spliceBlobRespDigest *rpb.Digest
	byteStreamReads      []string
	byteStreamWrites     []string
}

func (f *fakeChunkCAS) SplitBlob(ctx context.Context, req *rpb.SplitBlobRequest) (*rpb.SplitBlobResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.splitReqs = append(f.splitReqs, req)
	d := digest.FromProto(req.GetBlobDigest())
	chunks, ok := f.splitBlobMap[d]
	if !ok {
		return nil, status.Error(codes.NotFound, "no split info for blob")
	}
	return &rpb.SplitBlobResponse{
		ChunkDigests: chunks,
	}, nil
}

func (f *fakeChunkCAS) SpliceBlob(ctx context.Context, req *rpb.SpliceBlobRequest) (*rpb.SpliceBlobResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spliceReqs = append(f.spliceReqs, req)
	if f.spliceBlobErr != nil {
		return nil, f.spliceBlobErr
	}
	if f.spliceBlobRespDigest != nil {
		return &rpb.SpliceBlobResponse{
			BlobDigest: f.spliceBlobRespDigest,
		}, nil
	}
	var fullData []byte
	for _, cd := range req.GetChunkDigests() {
		d := digest.FromProto(cd)
		chunkBytes, ok := f.blobs[d]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "chunk %s not found in CAS", d)
		}
		fullData = append(fullData, chunkBytes...)
	}
	fullDigest := digest.SHA256.FromBytes(fullData)
	reqDigest := digest.FromProto(req.GetBlobDigest())
	if fullDigest != reqDigest {
		return nil, status.Errorf(codes.InvalidArgument, "splice digest mismatch: computed=%s requested=%s", fullDigest, reqDigest)
	}
	if f.blobs == nil {
		f.blobs = make(map[digest.Digest][]byte)
	}
	if f.splitBlobMap == nil {
		f.splitBlobMap = make(map[digest.Digest][]*rpb.Digest)
	}
	f.splitBlobMap[fullDigest] = slices.Clone(req.GetChunkDigests())
	f.blobs[fullDigest] = fullData
	return &rpb.SpliceBlobResponse{
		BlobDigest: req.GetBlobDigest(),
	}, nil
}

func (f *fakeChunkCAS) BatchReadBlobs(ctx context.Context, req *rpb.BatchReadBlobsRequest) (*rpb.BatchReadBlobsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchReadReqs = append(f.batchReadReqs, req)
	resp := &rpb.BatchReadBlobsResponse{}
	for _, dp := range req.GetDigests() {
		d := digest.FromProto(dp)
		data, ok := f.blobs[d]
		if !ok {
			resp.Responses = append(resp.Responses, &rpb.BatchReadBlobsResponse_Response{
				Digest: dp,
				Status: &statuspb.Status{
					Code:    int32(codes.NotFound),
					Message: fmt.Sprintf("blob %s not found", d),
				},
			})
			continue
		}
		resp.Responses = append(resp.Responses, &rpb.BatchReadBlobsResponse_Response{
			Digest:     dp,
			Data:       data,
			Compressor: rpb.Compressor_IDENTITY,
			Status: &statuspb.Status{
				Code: int32(codes.OK),
			},
		})
	}
	return resp, nil
}

func (f *fakeChunkCAS) BatchUpdateBlobs(ctx context.Context, req *rpb.BatchUpdateBlobsRequest) (*rpb.BatchUpdateBlobsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchUpdateReqs = append(f.batchUpdateReqs, req)
	resp := &rpb.BatchUpdateBlobsResponse{}
	for _, r := range req.GetRequests() {
		d := digest.FromProto(r.GetDigest())
		f.blobs[d] = r.GetData()
		resp.Responses = append(resp.Responses, &rpb.BatchUpdateBlobsResponse_Response{
			Digest: r.GetDigest(),
			Status: &statuspb.Status{
				Code: int32(codes.OK),
			},
		})
	}
	return resp, nil
}

func (f *fakeChunkCAS) FindMissingBlobs(ctx context.Context, req *rpb.FindMissingBlobsRequest) (*rpb.FindMissingBlobsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &rpb.FindMissingBlobsResponse{}
	for _, dp := range req.GetBlobDigests() {
		d := digest.FromProto(dp)
		if _, ok := f.blobs[d]; !ok {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, dp)
		}
	}
	return resp, nil
}

func (f *fakeChunkCAS) Read(req *bpb.ReadRequest, stream bpb.ByteStream_ReadServer) error {
	f.mu.Lock()
	f.byteStreamReads = append(f.byteStreamReads, req.GetResourceName())
	// Parse resource name: e.g. "{instance}/blobs/{hash}/{size}"
	parts := strings.Split(req.GetResourceName(), "/")
	var foundData []byte
	var found bool
	for d, data := range f.blobs {
		if strings.Contains(req.GetResourceName(), d.Hash) {
			foundData = data
			found = true
			break
		}
	}
	f.mu.Unlock()
	if !found {
		return status.Errorf(codes.NotFound, "resource %s not found in %v", req.GetResourceName(), parts)
	}
	return stream.Send(&bpb.ReadResponse{
		Data: foundData,
	})
}

func (f *fakeChunkCAS) Write(stream bpb.ByteStream_WriteServer) error {
	var buf bytes.Buffer
	var resourceName string
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if req.GetResourceName() != "" {
			resourceName = req.GetResourceName()
		}
		buf.Write(req.GetData())
		if req.GetFinishWrite() {
			break
		}
	}
	f.mu.Lock()
	f.byteStreamWrites = append(f.byteStreamWrites, resourceName)
	data := buf.Bytes()
	d := digest.SHA256.FromBytes(data)
	f.blobs[d] = data
	f.mu.Unlock()
	return stream.SendAndClose(&bpb.WriteResponse{
		CommittedSize: int64(buf.Len()),
	})
}

func (f *fakeChunkCAS) QueryWriteStatus(ctx context.Context, req *bpb.QueryWriteStatusRequest) (*bpb.QueryWriteStatusResponse, error) {
	return &bpb.QueryWriteStatusResponse{
		CommittedSize: 0,
		Complete:      false,
	}, nil
}

type fakeCapabilities struct {
	rpb.UnimplementedCapabilitiesServer

	capabilities *rpb.ServerCapabilities
}

func (f *fakeCapabilities) GetCapabilities(ctx context.Context, req *rpb.GetCapabilitiesRequest) (*rpb.ServerCapabilities, error) {
	if f.capabilities != nil {
		return f.capabilities, nil
	}
	return &rpb.ServerCapabilities{
		CacheCapabilities: &rpb.CacheCapabilities{
			DigestFunctions: []rpb.DigestFunction_Value{
				rpb.DigestFunction_SHA256,
			},
			ActionCacheUpdateCapabilities: &rpb.ActionCacheUpdateCapabilities{
				UpdateEnabled: true,
			},
			SplitBlobSupport:  true,
			SpliceBlobSupport: true,
		},
	}, nil
}

func setupChunkTestClientWithCapabilities(t *testing.T, fakeCAS *fakeChunkCAS, caps *rpb.ServerCapabilities, opt Option) (*Client, *LocalCache) {
	t.Helper()
	ctx := t.Context()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	rpb.RegisterContentAddressableStorageServer(srv, fakeCAS)
	bpb.RegisterByteStreamServer(srv, fakeCAS)
	rpb.RegisterCapabilitiesServer(srv, &fakeCapabilities{capabilities: caps})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	cacheDir := t.TempDir()
	localCache, err := NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}
	opt.LocalCache = localCache
	if opt.Instance == "" {
		opt.Instance = "projects/test/instances/default"
	}
	client, err := NewFromConn(ctx, opt, cred.Cred{}, conn, conn, conn)
	if err != nil {
		t.Fatalf("NewFromConn: %v", err)
	}
	err = client.Init(ctx)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return client, localCache
}

func setupChunkTestClient(t *testing.T, fakeCAS *fakeChunkCAS, opt Option) (*Client, *LocalCache) {
	t.Helper()
	return setupChunkTestClientWithCapabilities(t, fakeCAS, nil, opt)
}

func makeChunkDigests(sizes ...int64) []*rpb.Digest {
	var res []*rpb.Digest
	for i, sz := range sizes {
		d := digest.Digest{
			Hash:      fmt.Sprintf("hash-%d", i),
			SizeBytes: sz,
		}
		res = append(res, d.Proto())
	}
	return res
}

func TestChunkReadBatch_Empty(t *testing.T) {
	cacheDir := t.TempDir()
	localCache, err := NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}
	c := &Client{
		opt: Option{
			LocalCache:          localCache,
			chunkBatchThreshold: 1000,
		},
		digestFn: digest.SHA256,
	}
	batches := slices.Collect(c.chunkReadBatch(t.Context(), nil))
	if len(batches) != 0 {
		t.Errorf("got %d batches, want 0", len(batches))
	}
}

func TestChunkReadBatch_SingleBatchBelowLimit(t *testing.T) {
	cacheDir := t.TempDir()
	localCache, err := NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}
	c := &Client{
		opt: Option{
			LocalCache:          localCache,
			chunkBatchThreshold: 1000,
		},
		digestFn: digest.SHA256,
	}
	digests := makeChunkDigests(100, 200, 300)
	batches := slices.Collect(c.chunkReadBatch(t.Context(), digests))
	if len(batches) != 1 {
		t.Fatalf("got %d batches, want 1", len(batches))
	}
	if len(batches[0]) != 3 {
		t.Errorf("got %d chunks in batch 0, want 3", len(batches[0]))
	}
}

func TestChunkReadBatch_MultipleBatchesAboveLimit(t *testing.T) {
	cacheDir := t.TempDir()
	localCache, err := NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}
	c := &Client{
		opt: Option{
			LocalCache:          localCache,
			chunkBatchThreshold: 1000,
		},
		digestFn: digest.SHA256,
	}
	digests := makeChunkDigests(400, 400, 400, 400) // 4 chunks of 400 with limit 1000 -> 2 batches
	batches := slices.Collect(c.chunkReadBatch(t.Context(), digests))
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2", len(batches))
	}
	if len(batches[0]) != 2 || len(batches[1]) != 2 {
		t.Errorf("got batch sizes [%d, %d], want [2, 2]", len(batches[0]), len(batches[1]))
	}
}

func TestChunkReadBatch_SingleLargeChunkAboveLimit(t *testing.T) {
	cacheDir := t.TempDir()
	localCache, err := NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}
	c := &Client{
		opt: Option{
			LocalCache:          localCache,
			chunkBatchThreshold: 1000,
		},
		digestFn: digest.SHA256,
	}
	digests := makeChunkDigests(2000)
	batches := slices.Collect(c.chunkReadBatch(t.Context(), digests))
	if len(batches) != 1 {
		t.Fatalf("got %d batches, want 1", len(batches))
	}
	if len(batches[0]) != 1 {
		t.Errorf("got %d chunks, want 1", len(batches[0]))
	}
}

func TestChunkReadBatch_MixedChunkSizes(t *testing.T) {
	cacheDir := t.TempDir()
	localCache, err := NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}
	c := &Client{
		opt: Option{
			LocalCache:          localCache,
			chunkBatchThreshold: 1000,
		},
		digestFn: digest.SHA256,
	}
	digests := makeChunkDigests(300, 400, 500, 2000, 300, 400)
	batches := slices.Collect(c.chunkReadBatch(t.Context(), digests))
	// [300, 400] (700 < 1000, adding 500 exceeds 1000 -> yield [300, 400])
	// [500] (500 < 1000, adding 2000 exceeds 1000 -> yield [500])
	// [2000] (single chunk >= 1000 -> yield [2000])
	// [300, 400] (remaining -> yield [300, 400])
	if len(batches) != 4 {
		t.Fatalf("got %d batches, want 4", len(batches))
	}
	if len(batches[0]) != 2 || len(batches[1]) != 1 || len(batches[2]) != 1 || len(batches[3]) != 2 {
		t.Errorf("got batch sizes [%d, %d, %d, %d], want [2, 1, 1, 2]",
			len(batches[0]), len(batches[1]), len(batches[2]), len(batches[3]))
	}
}

func TestChunkReader_DisabledNoLocalCache(t *testing.T) {
	ctx := t.Context()
	fakeCAS := &fakeChunkCAS{
		splitBlobMap: make(map[digest.Digest][]*rpb.Digest),
		blobs:        make(map[digest.Digest][]byte),
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{})
	client.opt.LocalCache = nil

	d := digest.Digest{Hash: "test-hash", SizeBytes: 100}
	_, err := client.chunkReader(ctx, d, "file.txt")
	if err == nil {
		t.Fatal("chunkReader succeeded without local cache; want error")
	}
}

func TestChunkReader_SplitBlobNotFound(t *testing.T) {
	ctx := t.Context()
	fakeCAS := &fakeChunkCAS{
		splitBlobMap: make(map[digest.Digest][]*rpb.Digest),
		blobs:        make(map[digest.Digest][]byte),
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{})

	d := digest.Digest{Hash: "test-hash", SizeBytes: 100}
	_, err := client.chunkReader(ctx, d, "file.txt")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("chunkReader got err=%v, want NotFound", err)
	}
}

func TestChunkReader_BytestreamioFallbackOnNotFound(t *testing.T) {
	ctx := t.Context()
	raw := []byte("hello bytestream fallback world")
	d := digest.SHA256.FromBytes(raw)

	fakeCAS := &fakeChunkCAS{
		splitBlobMap: make(map[digest.Digest][]*rpb.Digest), // no split info -> returns NotFound
		blobs: map[digest.Digest][]byte{
			d: raw,
		},
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{
		ChunkedBlobsThreshold: 0,
	})

	rc, err := client.bytestreamioOpen(ctx, d, "fallback.txt")
	if err != nil {
		t.Fatalf("bytestreamioOpen: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("got %q, want %q", string(got), string(raw))
	}
	if len(fakeCAS.splitReqs) != 1 {
		t.Errorf("got %d splitReqs, want 1", len(fakeCAS.splitReqs))
	}
	if len(fakeCAS.byteStreamReads) != 1 {
		t.Errorf("got %d byteStreamReads, want 1", len(fakeCAS.byteStreamReads))
	}
}

func TestChunkReader_SuccessMultiChunk(t *testing.T) {
	ctx := t.Context()
	chunkData0 := bytes.Repeat([]byte("A"), 40*1024)
	chunkData1 := bytes.Repeat([]byte("B"), 40*1024)
	chunkData2 := bytes.Repeat([]byte("C"), 20*1024)
	fullData := append(append(slices.Clone(chunkData0), chunkData1...), chunkData2...)

	d0 := digest.SHA256.FromBytes(chunkData0)
	d1 := digest.SHA256.FromBytes(chunkData1)
	d2 := digest.SHA256.FromBytes(chunkData2)
	fullDigest := digest.SHA256.FromBytes(fullData)

	fakeCAS := &fakeChunkCAS{
		splitBlobMap: map[digest.Digest][]*rpb.Digest{
			fullDigest: {d0.Proto(), d1.Proto(), d2.Proto()},
		},
		blobs: map[digest.Digest][]byte{
			d0: chunkData0,
			d1: chunkData1,
			d2: chunkData2,
		},
	}
	client, localCache := setupChunkTestClient(t, fakeCAS, Option{
		CompressedBlob: 1024 * 1024,
	})

	r, err := client.chunkReader(ctx, fullDigest, "multi.dat")
	if err != nil {
		t.Fatalf("chunkReader: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, fullData) {
		t.Fatalf("content mismatch: got %d bytes, want %d bytes", len(got), len(fullData))
	}

	// Verify all chunks are cached locally
	for i, d := range []digest.Digest{d0, d1, d2} {
		if !localCache.HasContent(ctx, d) {
			t.Errorf("chunk %d (%s) not found in local cache", i, d)
		}
	}
}

func TestChunkReader_PreCachedChunks(t *testing.T) {
	ctx := t.Context()
	chunkData0 := []byte("ChunkZeroData")
	chunkData1 := []byte("ChunkOneData")
	chunkData2 := []byte("ChunkTwoData")
	fullData := append(append(slices.Clone(chunkData0), chunkData1...), chunkData2...)

	d0 := digest.SHA256.FromBytes(chunkData0)
	d1 := digest.SHA256.FromBytes(chunkData1)
	d2 := digest.SHA256.FromBytes(chunkData2)
	fullDigest := digest.SHA256.FromBytes(fullData)

	fakeCAS := &fakeChunkCAS{
		splitBlobMap: map[digest.Digest][]*rpb.Digest{
			fullDigest: {d0.Proto(), d1.Proto(), d2.Proto()},
		},
		blobs: map[digest.Digest][]byte{
			d0: chunkData0,
			d1: chunkData1,
			d2: chunkData2,
		},
	}
	client, localCache := setupChunkTestClient(t, fakeCAS, Option{
		ByteStreamReadThreshold: 1 * 1024 * 1024,
		CompressedBlob:          1024 * 1024,
	})

	// Pre-populate Chunk 0 and Chunk 2 in localCache
	if err := localCache.SetContent(ctx, d0, "precached#0", chunkData0); err != nil {
		t.Fatalf("SetContent d0: %v", err)
	}
	if err := localCache.SetContent(ctx, d2, "precached#2", chunkData2); err != nil {
		t.Fatalf("SetContent d2: %v", err)
	}

	r, err := client.chunkReader(ctx, fullDigest, "precached.dat")
	if err != nil {
		t.Fatalf("chunkReader: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, fullData) {
		t.Fatalf("content mismatch: got %q, want %q", string(got), string(fullData))
	}

	// Verify only Chunk 1 was requested from CAS
	var requestedDigests []digest.Digest
	for _, req := range fakeCAS.batchReadReqs {
		for _, dp := range req.GetDigests() {
			requestedDigests = append(requestedDigests, digest.FromProto(dp))
		}
	}
	if len(requestedDigests) != 1 || requestedDigests[0] != d1 {
		t.Errorf("requested digests = %v; want only [%s]", requestedDigests, d1)
	}
}

func TestChunkReader_SingleLargeChunk(t *testing.T) {
	ctx := t.Context()
	chunkData := bytes.Repeat([]byte("L"), 200*1024)
	d := digest.SHA256.FromBytes(chunkData)

	fakeCAS := &fakeChunkCAS{
		splitBlobMap: map[digest.Digest][]*rpb.Digest{
			d: {d.Proto()},
		},
		blobs: map[digest.Digest][]byte{
			d: chunkData,
		},
	}
	client, localCache := setupChunkTestClient(t, fakeCAS, Option{
		CompressedBlob: 50 * 1024, // batch limit 50KB, chunk is 200KB so chunkReadBatch yields len(chunks)==1
	})

	r, err := client.chunkReader(ctx, d, "large_chunk.dat")
	if err != nil {
		t.Fatalf("chunkReader: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, chunkData) {
		t.Fatalf("content mismatch: got %d bytes, want %d bytes", len(got), len(chunkData))
	}
	if !localCache.HasContent(ctx, d) {
		t.Errorf("chunk %s not found in local cache", d)
	}
}

func TestChunkReader_VaryingBufferSizes(t *testing.T) {
	ctx := t.Context()
	chunk0 := []byte("0123456789")
	chunk1 := []byte("abcdefghij")
	chunk2 := []byte("klmnopqrst")
	fullData := append(append(slices.Clone(chunk0), chunk1...), chunk2...)

	d0 := digest.SHA256.FromBytes(chunk0)
	d1 := digest.SHA256.FromBytes(chunk1)
	d2 := digest.SHA256.FromBytes(chunk2)
	fullDigest := digest.SHA256.FromBytes(fullData)

	for _, bufSize := range []int{1, 3, 7, 10, 15, 30, 100} {
		t.Run(fmt.Sprintf("bufSize_%d", bufSize), func(t *testing.T) {
			fakeCAS := &fakeChunkCAS{
				splitBlobMap: map[digest.Digest][]*rpb.Digest{
					fullDigest: {d0.Proto(), d1.Proto(), d2.Proto()},
				},
				blobs: map[digest.Digest][]byte{
					d0: chunk0,
					d1: chunk1,
					d2: chunk2,
				},
			}
			client, _ := setupChunkTestClient(t, fakeCAS, Option{
				CompressedBlob: 1000,
			})

			r, err := client.chunkReader(ctx, fullDigest, "vary.dat")
			if err != nil {
				t.Fatalf("chunkReader: %v", err)
			}
			defer r.Close()

			var buf bytes.Buffer
			tmp := make([]byte, bufSize)
			for {
				n, err := r.Read(tmp)
				if n > 0 {
					buf.Write(tmp[:n])
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Read error: %v", err)
				}
			}
			if !bytes.Equal(buf.Bytes(), fullData) {
				t.Errorf("got %q, want %q", buf.String(), string(fullData))
			}
		})
	}
}

func TestChunkReader_DigestMismatch(t *testing.T) {
	ctx := t.Context()
	chunkData0 := []byte("ValidChunk0")
	chunkData1 := []byte("ValidChunk1")
	corruptedData1 := []byte("CorruptChk1")
	fullData := append(slices.Clone(chunkData0), chunkData1...)

	d0 := digest.SHA256.FromBytes(chunkData0)
	corruptD1 := digest.SHA256.FromBytes(corruptedData1)
	fullDigest := digest.SHA256.FromBytes(fullData)

	fakeCAS := &fakeChunkCAS{
		splitBlobMap: map[digest.Digest][]*rpb.Digest{
			fullDigest: {d0.Proto(), corruptD1.Proto()},
		},
		blobs: map[digest.Digest][]byte{
			d0:        chunkData0,
			corruptD1: corruptedData1,
		},
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{
		CompressedBlob: 1000,
	})

	r, err := client.chunkReader(ctx, fullDigest, "corrupt.dat")
	if err != nil {
		t.Fatalf("chunkReader: %v", err)
	}
	defer r.Close()

	_, err = io.ReadAll(r)
	if err == nil {
		t.Fatal("expected digest mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "chunked blob digest mismatch") {
		t.Errorf("got err=%v, want 'chunked blob digest mismatch'", err)
	}
}

func TestChunkReader_SizeMismatch(t *testing.T) {
	ctx := t.Context()
	chunkData0 := []byte("Short")
	d0 := digest.SHA256.FromBytes(chunkData0)
	// Declare expected size as 100 bytes even though chunk is only 5 bytes
	fullDigest := digest.Digest{Hash: "somehash", SizeBytes: 100}

	fakeCAS := &fakeChunkCAS{
		splitBlobMap: map[digest.Digest][]*rpb.Digest{
			fullDigest: {d0.Proto()},
		},
		blobs: map[digest.Digest][]byte{
			d0: chunkData0,
		},
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{
		CompressedBlob: 1000,
	})

	r, err := client.chunkReader(ctx, fullDigest, "mismatch.dat")
	if err != nil {
		t.Fatalf("chunkReader: %v", err)
	}
	defer r.Close()

	_, err = io.ReadAll(r)
	if err == nil {
		t.Fatal("expected size mismatch / digest mismatch error, got nil")
	}
}

func TestChunkReader_Close(t *testing.T) {
	ctx := t.Context()
	chunkData0 := bytes.Repeat([]byte("X"), 1024)
	chunkData1 := bytes.Repeat([]byte("Y"), 1024)
	fullData := append(slices.Clone(chunkData0), chunkData1...)

	d0 := digest.SHA256.FromBytes(chunkData0)
	d1 := digest.SHA256.FromBytes(chunkData1)
	fullDigest := digest.SHA256.FromBytes(fullData)

	fakeCAS := &fakeChunkCAS{
		splitBlobMap: map[digest.Digest][]*rpb.Digest{
			fullDigest: {d0.Proto(), d1.Proto()},
		},
		blobs: map[digest.Digest][]byte{
			d0: chunkData0,
			d1: chunkData1,
		},
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{
		CompressedBlob: 10000,
	})

	r, err := client.chunkReader(ctx, fullDigest, "close.dat")
	if err != nil {
		t.Fatalf("chunkReader: %v", err)
	}

	buf := make([]byte, 100)
	n, err := r.Read(buf)
	if err != nil || n != 100 {
		t.Fatalf("Read: n=%d err=%v", n, err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.chunks != nil {
		t.Errorf("r.chunks not nilled after Close: %v", r.chunks)
	}
	if r.rd != nil {
		t.Errorf("r.rd not nilled after Close: %v", r.rd)
	}
}

func TestChunkUpload_DisabledNoLocalCache(t *testing.T) {
	ctx := t.Context()
	fakeCAS := &fakeChunkCAS{
		blobs: make(map[digest.Digest][]byte),
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{})
	client.opt.LocalCache = nil

	data := []byte("test data")
	d := digest.SHA256.FromBytes(data)
	err := client.chunkUpload(ctx, d, "upload.dat", bytes.NewReader(data))
	if err == nil {
		t.Fatal("chunkUpload succeeded without local cache; want error")
	}
	if !strings.Contains(err.Error(), "no local cache") {
		t.Errorf("got err=%v, want 'no local cache'", err)
	}
}

func generateTestData(size int) []byte {
	rnd := rand.NewChaCha8([32]byte{})
	data := make([]byte, size)
	rnd.Read(data)
	return data
}

func TestChunkUpload_SuccessMultiChunk(t *testing.T) {
	ctx := t.Context()
	fakeCAS := &fakeChunkCAS{
		blobs: make(map[digest.Digest][]byte),
	}
	// Use small AvgChunkSizeBytes so data creates multiple chunks
	caps := &rpb.ServerCapabilities{
		CacheCapabilities: &rpb.CacheCapabilities{
			DigestFunctions: []rpb.DigestFunction_Value{
				rpb.DigestFunction_SHA256,
			},
			SplitBlobSupport:  true,
			SpliceBlobSupport: true,
			FastCdc_2020Params: &rpb.FastCdc2020Params{
				AvgChunkSizeBytes: 1024,
			},
		},
	}
	client, localCache := setupChunkTestClientWithCapabilities(t, fakeCAS, caps, Option{})

	data := generateTestData(16384)
	fullDigest := digest.SHA256.FromBytes(data)

	err := client.chunkUpload(ctx, fullDigest, "upload_multi.dat", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("chunkUpload: %v", err)
	}

	if len(fakeCAS.spliceReqs) != 1 {
		t.Fatalf("got %d spliceReqs, want 1", len(fakeCAS.spliceReqs))
	}
	req := fakeCAS.spliceReqs[0]
	if req.GetInstanceName() != client.Instance() {
		t.Errorf("instance name = %q, want %q", req.GetInstanceName(), client.Instance())
	}
	if reqD := digest.FromProto(req.GetBlobDigest()); reqD != fullDigest {
		t.Errorf("requested blob digest = %v, want %v", reqD, fullDigest)
	}
	if req.GetDigestFunction() != rpb.DigestFunction_SHA256 {
		t.Errorf("digest function = %v, want %v", req.GetDigestFunction(), rpb.DigestFunction_SHA256)
	}
	if req.GetChunkingFunction() != rpb.ChunkingFunction_FAST_CDC_2020 {
		t.Errorf("chunking function = %v, want %v", req.GetChunkingFunction(), rpb.ChunkingFunction_FAST_CDC_2020)
	}
	if len(req.GetChunkDigests()) <= 1 {
		t.Fatalf("got %d chunk digests, want > 1", len(req.GetChunkDigests()))
	}

	// Verify all chunks are cached locally
	var chunkDigests []digest.Digest
	for i, cdp := range req.GetChunkDigests() {
		cd := digest.FromProto(cdp)
		chunkDigests = append(chunkDigests, cd)
		if !localCache.HasContent(ctx, cd) {
			t.Errorf("chunk %d (%s) not found in local cache", i, cd)
		}
	}

	// Verify all chunks and the full spliced blob exist in CAS via CAS Missing API (FindMissingBlobs)
	allDigests := append([]digest.Digest{fullDigest}, chunkDigests...)
	missing, err := client.Missing(ctx, allDigests)
	if err != nil {
		t.Fatalf("client.Missing: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("expected all blobs to exist in CAS, but missing: %v", missing)
	}

	// Verify the uploaded blob can be read back via CAS chunkReader API
	r, err := client.chunkReader(ctx, fullDigest, "download_multi.dat")
	if err != nil {
		t.Fatalf("chunkReader: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll from chunkReader: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("chunkReader content mismatch: got %d bytes, want %d bytes", len(got), len(data))
	}
}

func TestChunkUpload_PreCachedChunks(t *testing.T) {
	ctx := t.Context()
	fakeCAS := &fakeChunkCAS{
		blobs: make(map[digest.Digest][]byte),
	}
	caps := &rpb.ServerCapabilities{
		CacheCapabilities: &rpb.CacheCapabilities{
			DigestFunctions: []rpb.DigestFunction_Value{
				rpb.DigestFunction_SHA256,
			},
			SplitBlobSupport:  true,
			SpliceBlobSupport: true,
			FastCdc_2020Params: &rpb.FastCdc2020Params{
				AvgChunkSizeBytes: 1024,
			},
		},
	}
	client, localCache := setupChunkTestClientWithCapabilities(t, fakeCAS, caps, Option{})

	data := generateTestData(32768)
	fullDigest := digest.SHA256.FromBytes(data)

	// Pre-chunk data to find chunk digests
	chunker, err := client.chunker(rpb.ChunkingFunction_FAST_CDC_2020)
	if err != nil {
		t.Fatalf("chunker: %v", err)
	}
	var chunks []digest.Digest
	for chd, err := range chunker.Chunks(bytes.NewReader(data)) {
		if err != nil {
			t.Fatalf("chunk: %v", err)
		}
		cd := digest.SHA256.FromBytes(chd.Data)
		chunks = append(chunks, cd)
		if len(chunks) == 1 {
			// Pre-cache first chunk in localCache AND CAS
			if err := localCache.SetContent(ctx, cd, "chunk0", chd.Data); err != nil {
				t.Fatalf("SetContent chunk0: %v", err)
			}
			fakeCAS.blobs[cd] = chd.Data
		} else if len(chunks) == 2 {
			// Pre-cache second chunk ONLY in localCache (not in CAS)
			if err := localCache.SetContent(ctx, cd, "chunk1", chd.Data); err != nil {
				t.Fatalf("SetContent chunk1: %v", err)
			}
		}
	}
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(chunks))
	}

	err = client.chunkUpload(ctx, fullDigest, "precached_upload.dat", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("chunkUpload: %v", err)
	}

	if len(fakeCAS.spliceReqs) != 1 {
		t.Fatalf("got %d spliceReqs, want 1", len(fakeCAS.spliceReqs))
	}

	// Verify all chunks and the full spliced blob exist in CAS via CAS Missing API (FindMissingBlobs)
	missing, err := client.Missing(ctx, append([]digest.Digest{fullDigest}, chunks...))
	if err != nil {
		t.Fatalf("client.Missing: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("expected all blobs to exist in CAS, but missing: %v", missing)
	}

	// Verify chunk 0 was NOT re-uploaded to CAS (since it was already in fakeCAS.blobs)
	var uploadedDigests []digest.Digest
	for _, req := range fakeCAS.batchUpdateReqs {
		for _, r := range req.GetRequests() {
			uploadedDigests = append(uploadedDigests, digest.FromProto(r.GetDigest()))
		}
	}
	if slices.Contains(uploadedDigests, chunks[0]) {
		t.Errorf("chunk 0 (%s) was uploaded to CAS even though it was already present", chunks[0])
	}
	if !slices.Contains(uploadedDigests, chunks[1]) {
		t.Errorf("chunk 1 (%s) was not uploaded to CAS even though CAS was missing it", chunks[1])
	}

	// Verify reading back via CAS chunkReader API
	r, err := client.chunkReader(ctx, fullDigest, "download_precached.dat")
	if err != nil {
		t.Fatalf("chunkReader: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll from chunkReader: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("chunkReader content mismatch: got %d bytes, want %d bytes", len(got), len(data))
	}
}

func TestChunkUpload_SpliceBlobError(t *testing.T) {
	ctx := t.Context()
	fakeCAS := &fakeChunkCAS{
		blobs:         make(map[digest.Digest][]byte),
		spliceBlobErr: status.Error(codes.Internal, "simulated splice error"),
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{})

	data := []byte("splice error test data")
	fullDigest := digest.SHA256.FromBytes(data)

	err := client.chunkUpload(ctx, fullDigest, "splice_err.dat", bytes.NewReader(data))
	if err == nil {
		t.Fatal("chunkUpload succeeded; want error")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("status code = %v, want %v", status.Code(err), codes.Internal)
	}
}

func TestChunkUpload_SpliceBlobDigestMismatch(t *testing.T) {
	ctx := t.Context()
	wrongDigest := digest.Digest{Hash: "wronghash0123456789abcdef0123456789abcdef", SizeBytes: 999}
	fakeCAS := &fakeChunkCAS{
		blobs:                make(map[digest.Digest][]byte),
		spliceBlobRespDigest: wrongDigest.Proto(),
	}
	client, _ := setupChunkTestClient(t, fakeCAS, Option{})

	data := []byte("mismatch test data")
	fullDigest := digest.SHA256.FromBytes(data)

	err := client.chunkUpload(ctx, fullDigest, "mismatch.dat", bytes.NewReader(data))
	if err == nil {
		t.Fatal("chunkUpload succeeded; want mismatch error")
	}
	if !strings.Contains(err.Error(), "mismatch blob digest in SpliceBlob") {
		t.Errorf("got err=%v, want 'mismatch blob digest in SpliceBlob'", err)
	}
}

func TestChunkUpload_BytestreamioUpload_Chunked(t *testing.T) {
	ctx := t.Context()
	fakeCAS := &fakeChunkCAS{
		blobs: make(map[digest.Digest][]byte),
	}
	caps := &rpb.ServerCapabilities{
		CacheCapabilities: &rpb.CacheCapabilities{
			DigestFunctions: []rpb.DigestFunction_Value{
				rpb.DigestFunction_SHA256,
			},
			SplitBlobSupport:  true,
			SpliceBlobSupport: true,
			FastCdc_2020Params: &rpb.FastCdc2020Params{
				AvgChunkSizeBytes: 1024,
			},
		},
	}
	client, _ := setupChunkTestClientWithCapabilities(t, fakeCAS, caps, Option{
		ChunkedBlobsThreshold: 100,
	})

	data := generateTestData(24576)
	fullDigest := digest.SHA256.FromBytes(data)

	err := client.bytestreamioUpload(ctx, fullDigest, "bytestream_chunked.dat", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("bytestreamioUpload: %v", err)
	}
	if len(fakeCAS.spliceReqs) != 1 {
		t.Errorf("got %d spliceReqs, want 1", len(fakeCAS.spliceReqs))
	}

	// Verify via CAS API bytestreamioOpen
	r, err := client.bytestreamioOpen(ctx, fullDigest, "read_chunked.dat")
	if err != nil {
		t.Fatalf("bytestreamioOpen: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll from bytestreamioOpen: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("bytestreamioOpen content mismatch: got %d bytes, want %d bytes", len(got), len(data))
	}
}

func TestChunkUpload_BytestreamioUpload_FallbackWhenNotSupported(t *testing.T) {
	ctx := t.Context()
	fakeCAS := &fakeChunkCAS{
		blobs: make(map[digest.Digest][]byte),
	}
	caps := &rpb.ServerCapabilities{
		CacheCapabilities: &rpb.CacheCapabilities{
			DigestFunctions: []rpb.DigestFunction_Value{
				rpb.DigestFunction_SHA256,
			},
			SplitBlobSupport:  false,
			SpliceBlobSupport: false,
		},
	}
	client, _ := setupChunkTestClientWithCapabilities(t, fakeCAS, caps, Option{
		ChunkedBlobsThreshold: 100,
	})

	data := generateTestData(8192)
	fullDigest := digest.SHA256.FromBytes(data)

	err := client.bytestreamioUpload(ctx, fullDigest, "bytestream_fallback.dat", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("bytestreamioUpload: %v", err)
	}
	if len(fakeCAS.spliceReqs) != 0 {
		t.Errorf("got %d spliceReqs, want 0", len(fakeCAS.spliceReqs))
	}
	if len(fakeCAS.byteStreamWrites) != 1 {
		t.Errorf("got %d byteStreamWrites, want 1", len(fakeCAS.byteStreamWrites))
	}

	// Verify via CAS API bytestreamioOpen
	r, err := client.bytestreamioOpen(ctx, fullDigest, "read_fallback.dat")
	if err != nil {
		t.Fatalf("bytestreamioOpen: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll from bytestreamioOpen: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("bytestreamioOpen content mismatch: got %d bytes, want %d bytes", len(got), len(data))
	}
}

func TestChunker(t *testing.T) {
	fakeCAS := &fakeChunkCAS{
		blobs: make(map[digest.Digest][]byte),
	}
	caps := &rpb.ServerCapabilities{
		CacheCapabilities: &rpb.CacheCapabilities{
			DigestFunctions: []rpb.DigestFunction_Value{
				rpb.DigestFunction_SHA256,
			},
			FastCdc_2020Params: &rpb.FastCdc2020Params{
				AvgChunkSizeBytes: 1024,
				Seed:              42,
			},
			RepMaxCdcParams: &rpb.RepMaxCdcParams{
				MinChunkSizeBytes: 512,
				HorizonSizeBytes:  2048,
			},
		},
	}
	client, _ := setupChunkTestClientWithCapabilities(t, fakeCAS, caps, Option{})

	// FastCDC with capability params
	fastChunker, err := client.chunker(rpb.ChunkingFunction_FAST_CDC_2020)
	if err != nil {
		t.Fatalf("chunker(FAST_CDC_2020): %v", err)
	}
	if fastChunker == nil {
		t.Fatal("fastChunker is nil")
	}

	// RepMaxCDC with capability params
	repMaxChunker, err := client.chunker(rpb.ChunkingFunction_REP_MAX_CDC)
	if err != nil {
		t.Fatalf("chunker(REP_MAX_CDC): %v", err)
	}
	if repMaxChunker == nil {
		t.Fatal("repMaxChunker is nil")
	}

	// Default params without capability params
	defaultClient, _ := setupChunkTestClient(t, fakeCAS, Option{})
	defaultFastChunker, err := defaultClient.chunker(rpb.ChunkingFunction_FAST_CDC_2020)
	if err != nil {
		t.Fatalf("default chunker(FAST_CDC_2020): %v", err)
	}
	if defaultFastChunker == nil {
		t.Fatal("defaultFastChunker is nil")
	}

	defaultRepMaxChunker, err := defaultClient.chunker(rpb.ChunkingFunction_REP_MAX_CDC)
	if err != nil {
		t.Fatalf("default chunker(REP_MAX_CDC): %v", err)
	}
	if defaultRepMaxChunker == nil {
		t.Fatal("defaultRepMaxChunker is nil")
	}

	// Unknown function
	_, err = client.chunker(rpb.ChunkingFunction_UNKNOWN)
	if err == nil {
		t.Fatal("chunker(UNKNOWN) succeeded; want error")
	}
}
