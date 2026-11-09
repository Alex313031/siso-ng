// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
)

func makeDigest(s string) digest.Digest {
	return blob.FromBytes(digest.SHA256, s, []byte(s)).Digest()
}

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

func TestProtoCache(t *testing.T) {
	ctx := t.Context()
	cache, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cmd := &rpb.Command{
		Arguments:        []string{"clang++", "-c", "foo.cc"},
		WorkingDirectory: "out/Default",
	}
	d := makeDigest("cmd-1")
	gotCmd := &rpb.Command{}
	err = cache.Proto(ctx, d, gotCmd)
	if err == nil {
		t.Errorf("Proto unexpectedly succeeded on empty cache")
	}

	err = cache.SetProto(ctx, d, cmd)
	if err != nil {
		t.Fatalf("SetProto: %v", err)
	}

	err = cache.Proto(ctx, d, gotCmd)
	if err != nil {
		t.Fatalf("Proto: %v", err)
	}
	if !proto.Equal(cmd, gotCmd) {
		t.Errorf("Proto = %v, want %v", gotCmd, cmd)
	}
}

func TestActionLookupCache(t *testing.T) {
	ctx := t.Context()
	cache, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	lookupKey := "lookup-key-1/size"
	action1 := makeDigest("action-1")
	action2 := makeDigest("action-2")

	// Empty list
	var found []digest.Digest
	for d, err := range cache.ListActionDigests(ctx, lookupKey) {
		if err != nil {
			t.Fatalf("ListActionDigests: %v", err)
		}
		found = append(found, d)
	}
	if len(found) != 0 {
		t.Errorf("ListActionDigests on empty = %v, want empty", found)
	}

	// Add actions
	err = cache.AddActionLookup(ctx, lookupKey, action1)
	if err != nil {
		t.Fatalf("AddActionLookup(action1): %v", err)
	}
	err = cache.AddActionLookup(ctx, lookupKey, action2)
	if err != nil {
		t.Fatalf("AddActionLookup(action2): %v", err)
	}

	found = nil
	for d, err := range cache.ListActionDigests(ctx, lookupKey) {
		if err != nil {
			t.Fatalf("ListActionDigests: %v", err)
		}
		found = append(found, d)
	}
	if len(found) != 2 {
		t.Fatalf("ListActionDigests returned %d actions, want 2: %v", len(found), found)
	}
}

func TestLocalActionCacheMap(t *testing.T) {
	ctx := t.Context()
	cache, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	acm := &ActionCacheMap{
		c: &Client{
			opt: Option{
				LocalCache:                    cache,
				DisableTwoPhaseCachingMethods: true,
			},
		},
	}
	lookupKey := "lookup-key-test/100"
	actionDigest := makeDigest("action-digest-1")

	wantAction := &rpb.Action{
		CommandDigest: makeDigest("cmd-digest-1").Proto(),
	}
	err = cache.SetProto(ctx, actionDigest, wantAction)
	if err != nil {
		t.Fatalf("SetProto: %v", err)
	}

	err = acm.Add(ctx, lookupKey, actionDigest)
	if err != nil {
		t.Fatalf("acm.Add: %v", err)
	}

	var actions []*rpb.Action
	for action, err := range acm.List(ctx, lookupKey) {
		if err != nil {
			t.Fatalf("acm.List: %v", err)
		}
		actions = append(actions, action)
	}
	if len(actions) != 1 {
		t.Fatalf("acm.List got %d actions, want 1", len(actions))
	}
	if !proto.Equal(actions[0], wantAction) {
		t.Errorf("acm.List action = %v, want %v", actions[0], wantAction)
	}
}
