// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"path/filepath"
	"testing"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"
)

func TestActionResultCache(t *testing.T) {
	ctx := t.Context()
	cache, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	digest := makeDigest("newly added")
	val, err := cache.GetActionResult(ctx, digest)
	if err == nil || val != nil {
		t.Errorf("GetActionResult unexpectedly succeeded on an empty cache")
	}

	want := &rpb.ActionResult{StdoutRaw: []byte("a")}

	if err := cache.SetActionResult(ctx, digest, want); err != nil {
		t.Errorf("cache.SetActionResult(%v, %v) = %v; want nil", digest, want, err)
	}

	got, err := cache.GetActionResult(ctx, digest)
	if err != nil || !proto.Equal(got, want) {
		t.Errorf("cache.GetActionResult(%v) = %v, %v; want %v, nil", digest, got, err, want)
	}
}

func TestGarbageCollector(t *testing.T) {
	ctx := t.Context()
	d := filepath.Join(t.TempDir(), "cache")
	cache, err := NewLocalCache(d)
	if err != nil {
		t.Fatal(err)
	}
	cache.timestamp = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	if cache.needsGarbageCollection(23 * time.Hour) {
		t.Errorf("cache.NeedsGarbageCollection(23 hours) = true; want false")
	}

	oneDayLater, err := NewLocalCache(d)
	if err != nil {
		t.Fatal(err)
	}
	oneDayLater.timestamp = time.Date(2024, time.January, 2, 0, 0, 0, 0, time.UTC)

	// It shouldn't need garbage collection since the cache doesn't yet exist on
	// disk.
	if oneDayLater.needsGarbageCollection(23 * time.Hour) {
		t.Errorf("oneDayLater.NeedsGarbageCollection(23 hours) = true; want false")
	}

	digest := makeDigest("newly added")
	if err := cache.SetContent(ctx, digest, "foo", []byte("foo")); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.GetContent(ctx, digest, "foo"); err != nil {
		t.Fatal(err)
	}

	if !oneDayLater.needsGarbageCollection(23 * time.Hour) {
		t.Errorf("oneDayLater.NeedsGarbageCollection(23 hours) = false; want true")
	}

	// Nothing should happen.
	oneDayLater.garbageCollect(ctx, 25*time.Hour)
	if !cache.HasContent(ctx, digest) {
		t.Errorf("cache.HasContent() should return true after no GC")
	}

	// Now they're expired since we garbage collect with a lower TTL
	oneDayLater.garbageCollect(ctx, 23*time.Hour)
	if cache.HasContent(ctx, digest) {
		t.Errorf("cache.HasContent() should return false after successful GC")
	}
}
