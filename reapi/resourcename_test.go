// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"strings"
	"testing"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"
)

// TestResourceNameDigestFunctionSegment guards that every resource-name builder
// includes the {digest_function} segment for non-sha256 functions (omitted for
// sha256). A regression here silently routes streamed blobs to the wrong CAS
// namespace on the server.
func TestResourceNameDigestFunctionSegment(t *testing.T) {
	d := digest.Digest{Hash: "a9993e364706816aba3e25717850c26c9cd0d89d", SizeBytes: 3}

	for _, tc := range []struct {
		fn          rpb.DigestFunction_Value
		wantSegment string // "" means no function segment expected
	}{
		{rpb.DigestFunction_SHA256, ""},
		{rpb.DigestFunction_SHA1, ""}, // legacy: segment omitted, inferred by length.
		{rpb.DigestFunction_GITSHA1, "gitsha1"},
		{rpb.DigestFunction_BLAKE3, "blake3"},
	} {
		fn, err := digest.Lookup(tc.fn)
		if err != nil {
			t.Fatalf("digest.Lookup(%v): %v", tc.fn, err)
		}
		c := &Client{opt: Option{Instance: "inst"}, digestFn: fn}

		read := c.resourceName(d)
		upload := c.uploadResourceName(d)
		uri := c.FileURI(d)

		for _, got := range []struct{ what, name string }{
			{"resourceName", read},
			{"uploadResourceName", upload},
			{"FileURI", uri},
		} {
			seg := "/" + tc.wantSegment + "/"
			switch tc.wantSegment {
			case "":
				// sha256: must be "/blobs/{hash}/..." with no function segment.
				if strings.Contains(got.name, "/sha1/") || strings.Contains(got.name, "/blake3/") || strings.Contains(got.name, "/gitsha1/") {
					t.Errorf("%s(%v) = %q, unexpectedly has a function segment", got.what, tc.fn, got.name)
				}
			default:
				if !strings.Contains(got.name, seg) {
					t.Errorf("%s(%v) = %q, missing segment %q", got.what, tc.fn, got.name, seg)
				}
			}
		}
	}
}
