// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"bytes"
	"fmt"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"
)

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("proto.Marshal(%v)=%v", m, err)
	}
	return b
}

// TestRawTreeResponseParseNextPageToken checks that parse reads
// next_page_token.
func TestRawTreeResponseParseNextPageToken(t *testing.T) {
	var resp rawTreeResponse
	if err := resp.parse(marshal(t, &rpb.GetTreeResponse{NextPageToken: "token"})); err != nil {
		t.Fatalf("parse=%v; want nil", err)
	}
	if resp.nextPageToken != "token" {
		t.Errorf("nextPageToken=%q; want %q", resp.nextPageToken, "token")
	}
}

// TestRawTreeResponseParseUnknownFields checks that parse skips fields
// it does not know.
func TestRawTreeResponseParseUnknownFields(t *testing.T) {
	dir1 := &rpb.Directory{Files: []*rpb.FileNode{{Name: "a"}}}
	dir2 := &rpb.Directory{Files: []*rpb.FileNode{{Name: "b"}}}
	b := marshal(t, &rpb.GetTreeResponse{Directories: []*rpb.Directory{dir1}})
	b = protowire.AppendTag(b, 100, protowire.VarintType)
	b = protowire.AppendVarint(b, 1)
	b = protowire.AppendTag(b, 101, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("unknown"))
	b = append(b, marshal(t, &rpb.GetTreeResponse{Directories: []*rpb.Directory{dir2}})...)

	var resp rawTreeResponse
	if err := resp.parse(b); err != nil {
		t.Fatalf("parse=%v; want nil", err)
	}
	want := [][]byte{marshal(t, dir1), marshal(t, dir2)}
	if len(resp.dirs) != len(want) {
		t.Fatalf("parse returned %d directories; want %d", len(resp.dirs), len(want))
	}
	for i := range want {
		if !bytes.Equal(resp.dirs[i], want[i]) {
			t.Errorf("dirs[%d]=%q; want %q", i, resp.dirs[i], want[i])
		}
	}
}

// TestWalkStateRelease checks that release clears a walkState before
// pooling it, and leaves one with an oversized map unpooled (and so
// uncleared).
func TestWalkStateRelease(t *testing.T) {
	newState := func(n int) *walkState {
		ws := &walkState{
			dirs:    make(map[string]digest.Digest),
			waiting: make(map[digest.Digest][]string),
			known:   make(map[digest.Digest]*rpb.Directory),
		}
		for i := range n {
			d := digest.SHA256.FromBytes(fmt.Appendf(nil, "%d", i))
			ws.dirs[fmt.Sprintf("d%d", i)] = d
			ws.waiting[d] = []string{"x"}
			ws.known[d] = &rpb.Directory{}
		}
		return ws
	}
	ws := newState(3)
	ws.release()
	if len(ws.dirs) != 0 || len(ws.waiting) != 0 || len(ws.known) != 0 {
		t.Errorf("after release: %d dirs, %d waiting, %d known; want 0", len(ws.dirs), len(ws.waiting), len(ws.known))
	}
	ws = newState(maxPooledWalkDirs + 1)
	ws.release()
	if len(ws.dirs) != maxPooledWalkDirs+1 {
		t.Errorf("after release of an oversized state: %d dirs; want %d", len(ws.dirs), maxPooledWalkDirs+1)
	}
}
