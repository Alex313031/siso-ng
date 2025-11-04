// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"io"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/build/cachestore"
	"go.chromium.org/build/siso/reapi/digest"
)

const FirstOnly = "firstonly"
const SecondOnly = "secondonly"
const Both = "both"
const Neither = "neither"
const NewlyAdded = "newly_added"
const ActionResult = "action_result"

func makeDigest(s string) digest.Digest {
	return digest.FromBytes(s, []byte(s)).Digest()
}

func setContent(ctx context.Context, cache cachestore.CacheStore, s string) error {
	return cache.SetContent(ctx, makeDigest(s), s, []byte(s))
}

func TestLayeredCache(t *testing.T) {
	ctx := t.Context()
	cache := NewLayeredCache()

	first, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := setContent(ctx, first, FirstOnly); err != nil {
		t.Fatal(err)
	}
	if err := setContent(ctx, first, Both); err != nil {
		t.Fatal(err)
	}
	cache.AddLayer(first)

	second, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := setContent(ctx, second, SecondOnly); err != nil {
		t.Fatal(err)
	}
	if err := setContent(ctx, second, Both); err != nil {
		t.Fatal(err)
	}

	if err := setContent(ctx, cache, NewlyAdded); err != nil {
		t.Fatal(err)
	}
	cache.AddLayer(second)

	if cache.HasContent(ctx, makeDigest(Neither)) {
		t.Errorf("HasContent(%v) = true, want false", Neither)
	}

	for _, tc := range []string{
		FirstOnly,
		SecondOnly,
		Both,
		NewlyAdded,
	} {
		if !cache.HasContent(ctx, makeDigest(tc)) {
			t.Errorf("HasContent(%v) = false, want true", tc)
		}
		gotContent, err := cache.GetContent(ctx, makeDigest(tc), tc)
		if err != nil {
			t.Error(err)
		}
		if string(gotContent) != tc {
			t.Errorf("GetContent(%v) = %v, want %v", tc, string(gotContent), tc)
		}

		f, err := cache.Source(ctx, makeDigest(tc), tc).Open(ctx)
		if err != nil {
			t.Error(err)
		}
		gotContent, err = io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Error(err)
		}
		if string(gotContent) != tc {
			t.Errorf("io.ReadAll(Source(%v).Open()) = %v, want %v", tc, gotContent, tc)
		}
	}

	if second.HasContent(ctx, makeDigest(FirstOnly)) {
		t.Errorf("second.HasContent(%v) = true, want false", FirstOnly)
	}
	if !first.HasContent(ctx, makeDigest(SecondOnly)) {
		t.Errorf("first.HasContent(%v) = false, want true", SecondOnly)
	}
}

func TestWriteThroughCache(t *testing.T) {
	ctx := t.Context()
	cache := NewLayeredCache()

	first, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache.AddLayer(first)

	second, err := NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	wantProto := &rpb.ActionResult{
		StdoutRaw:   []byte("a"),
		OutputFiles: []*rpb.OutputFile{{Path: "foo"}},
	}
	if err := second.SetActionResult(ctx, makeDigest(ActionResult), wantProto); err != nil {
		t.Fatal(err)
	}

	cache.AddLayer(second)

	// It should initially only exist in the second cache
	_, err = first.GetActionResult(ctx, makeDigest(ActionResult))
	if err == nil {
		t.Errorf("first.GetActionResult(%v) succeeded, want failure", ActionResult)
	}
	ar, err := second.GetActionResult(ctx, makeDigest(ActionResult))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(ar, wantProto) {
		t.Errorf("second.GetActionResult(%v) = %v, want %v", ActionResult, ar, wantProto)
	}

	ar, err = cache.GetActionResult(ctx, makeDigest(ActionResult))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(ar, wantProto) {
		t.Errorf("cache.GetActionResult(%v) = %v, want %v", ActionResult, ar, wantProto)
	}

	// While reading from the second layer of the cache, it should have written
	// to the first layer of the cache, so it should now exist.
	ar, err = first.GetActionResult(ctx, makeDigest(ActionResult))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(ar, wantProto) {
		t.Errorf("first.GetActionResult(%v) = %v, want %v", ActionResult, ar, wantProto)
	}
}
