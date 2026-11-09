// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resultstore

import (
	"testing"

	rspb "google.golang.org/genproto/googleapis/devtools/resultstore/v2"

	"go.chromium.org/build/hashigo/digest"
)

func TestHashType(t *testing.T) {
	for _, tc := range []struct {
		fn   string
		want rspb.File_HashType
	}{
		{"sha256", rspb.File_SHA256},
		{"sha1", rspb.File_SHA1},
		{"md5", rspb.File_MD5},
		// ResultStore only models MD5/SHA1/SHA256; anything else is unspecified.
		{"blake3", rspb.File_HASH_TYPE_UNSPECIFIED},
		{"gitsha1", rspb.File_HASH_TYPE_UNSPECIFIED},
		{"sha384", rspb.File_HASH_TYPE_UNSPECIFIED},
		{"sha512", rspb.File_HASH_TYPE_UNSPECIFIED},
		{"murmur3", rspb.File_HASH_TYPE_UNSPECIFIED},
		{"vso", rspb.File_HASH_TYPE_UNSPECIFIED},
		{"sha256tree", rspb.File_HASH_TYPE_UNSPECIFIED},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			fn, err := digest.ParseFunction(tc.fn)
			if err != nil {
				t.Fatalf("ParseFunction(%q): %v", tc.fn, err)
			}
			if got, want := hashType(fn), tc.want; got != want {
				t.Errorf("hashType(%s) = %v; want %v", fn, got, want)
			}
		})
	}
}
