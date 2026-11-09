// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi_test

import (
	"bytes"
	"io"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestCacheStore_ActionResult_LocalCache(t *testing.T) {
	ctx := t.Context()
	cacheDir := t.TempDir()
	localCache, err := reapi.NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}

	fake := &reapitest.Fake{}
	client := reapitest.NewWithOption(ctx, t, fake, reapi.Option{
		LocalCache: localCache,
	})

	cs := client.CacheStore()

	outData := []byte("output-content")
	outBlob := blob.FromBytes(digest.SHA256, "out.o", outData)
	actionData := []byte("action-command")
	actionBlob := blob.FromBytes(digest.SHA256, "action", actionData)
	actionDigest := actionBlob.Digest()

	ds := blob.NewStore()
	ds.Set(outBlob)
	ds.Set(actionBlob)
	_, err = client.UploadAll(ctx, ds)
	if err != nil {
		t.Fatalf("UploadAll: %v", err)
	}

	wantResult := &rpb.ActionResult{
		OutputFiles: []*rpb.OutputFile{{
			Path:   "out.o",
			Digest: outBlob.Digest().Proto(),
		}},
	}

	// Initially neither localCache nor remote AC has the result.
	_, err = cs.GetActionResult(ctx, actionDigest)
	if err == nil {
		t.Fatal("GetActionResult: got nil error, want NotFound")
	}

	// Populate remote AC by updating it.
	err = client.UpdateActionResult(ctx, actionDigest, wantResult)
	if err != nil {
		t.Fatalf("UpdateActionResult: %v", err)
	}

	// Verify local cache does not have it yet.
	_, err = localCache.GetActionResult(ctx, actionDigest)
	if err == nil {
		t.Fatal("localCache.GetActionResult: got nil error, want NotFound before write-through")
	}

	// Read through CacheStore: should fetch from remote and write through to localCache.
	gotResult, err := cs.GetActionResult(ctx, actionDigest)
	if err != nil {
		t.Fatalf("cs.GetActionResult: %v", err)
	}
	if !proto.Equal(gotResult, wantResult) {
		t.Errorf("cs.GetActionResult: got %v, want %v", gotResult, wantResult)
	}

	// Now localCache should have it.
	localResult, err := localCache.GetActionResult(ctx, actionDigest)
	if err != nil {
		t.Fatalf("localCache.GetActionResult after write-through: %v", err)
	}
	if !proto.Equal(localResult, wantResult) {
		t.Errorf("localCache.GetActionResult: got %v, want %v", localResult, wantResult)
	}

	// Test SetActionResult writes to localCache.
	newActionDigest := digest.SHA256.FromBytes([]byte("new-action-command"))
	newResult := &rpb.ActionResult{
		OutputFiles: []*rpb.OutputFile{{
			Path:   "new.o",
			Digest: outBlob.Digest().Proto(),
		}},
	}
	err = cs.SetActionResult(ctx, newActionDigest, newResult)
	if err != nil {
		t.Fatalf("cs.SetActionResult: %v", err)
	}
	gotLocalNew, err := localCache.GetActionResult(ctx, newActionDigest)
	if err != nil {
		t.Fatalf("localCache.GetActionResult for SetActionResult: %v", err)
	}
	if !proto.Equal(gotLocalNew, newResult) {
		t.Errorf("got %v, want %v", gotLocalNew, newResult)
	}
}

func TestCacheStore_Content_LocalCache(t *testing.T) {
	ctx := t.Context()
	cacheDir := t.TempDir()
	localCache, err := reapi.NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}

	fake := &reapitest.Fake{}
	client := reapitest.NewWithOption(ctx, t, fake, reapi.Option{
		LocalCache:              localCache,
		ByteStreamReadThreshold: 1024, // 1KB threshold for test
	})

	cs := client.CacheStore()

	// 1. Small content (< ByteStreamReadThreshold)
	smallData := []byte("small-blob-content")
	smallBlob := blob.FromBytes(digest.SHA256, "small.txt", smallData)
	ds := blob.NewStore()
	ds.Set(smallBlob)
	_, err = client.UploadAll(ctx, ds)
	if err != nil {
		t.Fatalf("UploadAll: %v", err)
	}

	// Verify not yet in local cache.
	if localCache.HasContent(ctx, smallBlob.Digest()) {
		t.Fatal("smallBlob should not be in localCache yet")
	}

	// GetContent should fetch from remote and write through to localCache.
	got, err := cs.GetContent(ctx, smallBlob.Digest(), "small.txt")
	if err != nil {
		t.Fatalf("cs.GetContent: %v", err)
	}
	if !bytes.Equal(got, smallData) {
		t.Errorf("cs.GetContent: got %q, want %q", got, smallData)
	}
	if !localCache.HasContent(ctx, smallBlob.Digest()) {
		t.Errorf("smallBlob should now be in localCache after GetContent")
	}

	// HasContent on CacheStore should report true via localCache.
	if !cs.HasContent(ctx, smallBlob.Digest()) {
		t.Errorf("cs.HasContent: got false, want true")
	}

	// 2. Large content (>= ByteStreamReadThreshold)
	largeData := bytes.Repeat([]byte("large-blob-data-chunk-"), 100) // ~2.2KB > 1KB
	largeBlob := blob.FromBytes(digest.SHA256, "large.txt", largeData)
	dsLarge := blob.NewStore()
	dsLarge.Set(largeBlob)
	_, err = client.UploadAll(ctx, dsLarge)
	if err != nil {
		t.Fatalf("UploadAll large: %v", err)
	}

	// Verify not yet in local cache.
	if localCache.HasContent(ctx, largeBlob.Digest()) {
		t.Fatal("largeBlob should not be in localCache yet")
	}

	// Read via cs.Source().Open(): should stream and write-through to localCache.
	src := cs.Source(ctx, largeBlob.Digest(), "large.txt")
	r, err := src.Open(ctx)
	if err != nil {
		t.Fatalf("src.Open: %v", err)
	}
	streamed, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(streamed, largeData) {
		t.Errorf("streamed content mismatch: got %d bytes, want %d", len(streamed), len(largeData))
	}

	// Verify now present in localCache.
	if !localCache.HasContent(ctx, largeBlob.Digest()) {
		t.Errorf("largeBlob should now be in localCache after Source().Open()")
	}

	// Next read of large content should be served directly from localCache.
	r2, err := src.Open(ctx)
	if err != nil {
		t.Fatalf("src.Open from localCache: %v", err)
	}
	streamed2, err := io.ReadAll(r2)
	r2.Close()
	if err != nil {
		t.Fatalf("ReadAll from localCache: %v", err)
	}
	if !bytes.Equal(streamed2, largeData) {
		t.Errorf("content from localCache mismatch: got %d bytes, want %d", len(streamed2), len(largeData))
	}
}

func TestCacheStore_SetContent_UploadOnly(t *testing.T) {
	ctx := t.Context()
	cacheDir := t.TempDir()
	localCache, err := reapi.NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}

	fake := &reapitest.Fake{}
	client := reapitest.NewWithOption(ctx, t, fake, reapi.Option{
		LocalCache: localCache,
	})

	cs := client.CacheStore()
	data := []byte("content-to-set")
	d := digest.SHA256.FromBytes(data)

	err = cs.SetContent(ctx, d, "content.txt", data)
	if err != nil {
		t.Fatalf("cs.SetContent: %v", err)
	}

	// SetContent is an upload path to remote CAS and does not write to localCache.
	if localCache.HasContent(ctx, d) {
		t.Errorf("localCache should not have content after SetContent")
	}

	// Verify written to remote CAS.
	got, err := fake.Fetch(ctx, d.Proto())
	if err != nil {
		t.Fatalf("fake.Fetch: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("fake.Fetch: got %q, want %q", got, data)
	}
}

func TestBytestreamioOpen_LocalCache(t *testing.T) {
	ctx := t.Context()
	cacheDir := t.TempDir()
	localCache, err := reapi.NewLocalCache(cacheDir)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}

	fake := &reapitest.Fake{}
	client := reapitest.NewWithOption(ctx, t, fake, reapi.Option{
		LocalCache:              localCache,
		ByteStreamReadThreshold: 1024,
	})

	data := []byte("bytestream-content-data")
	b := blob.FromBytes(digest.SHA256, "file.txt", data)
	d := b.Digest()

	ds := blob.NewStore()
	ds.Set(b)
	_, err = client.UploadAll(ctx, ds)
	if err != nil {
		t.Fatalf("UploadAll: %v", err)
	}

	// Verify not yet in localCache.
	if localCache.HasContent(ctx, d) {
		t.Fatal("localCache should not have content yet")
	}

	// Read via client.GetReader (which calls digestSource.Open -> bytestreamioOpen).
	// Should read from ByteStream and write through into localCache.
	r, err := client.GetReader(ctx, d, "file.txt")
	if err != nil {
		t.Fatalf("client.GetReader: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("got %q, want %q", got, data)
	}

	// Verify it was written into localCache.
	if !localCache.HasContent(ctx, d) {
		t.Errorf("localCache should now have content after reading through bytestreamioOpen")
	}

	cachedContent, err := localCache.GetContent(ctx, d, "file.txt")
	if err != nil {
		t.Fatalf("localCache.GetContent: %v", err)
	}
	if !bytes.Equal(cachedContent, data) {
		t.Errorf("cached content mismatch: got %q, want %q", cachedContent, data)
	}

	// Now test when blob exists only in localCache and NOT in remote CAS.
	data2 := []byte("cached-only-content")
	d2 := digest.SHA256.FromBytes(data2)
	err = localCache.SetContent(ctx, d2, "cached_only.txt", data2)
	if err != nil {
		t.Fatalf("localCache.SetContent: %v", err)
	}

	r2, err := client.GetReader(ctx, d2, "cached_only.txt")
	if err != nil {
		t.Fatalf("client.GetReader for locally cached content: %v", err)
	}
	got2, err := io.ReadAll(r2)
	r2.Close()
	if err != nil {
		t.Fatalf("ReadAll for locally cached content: %v", err)
	}
	if !bytes.Equal(got2, data2) {
		t.Errorf("got %q, want %q", got2, data2)
	}
}
