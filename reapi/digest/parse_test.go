// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package digest

import "testing"

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		input string
		want  Digest
	}{
		{
			name:  "hash/size",
			input: "6400fa014f9e835db82d6f27fb71e100d623eba0ab346fa890304412367e798c/310",
			want: Digest{
				Hash:      "6400fa014f9e835db82d6f27fb71e100d623eba0ab346fa890304412367e798c",
				SizeBytes: 310,
			},
		},
		{
			name:  "json",
			input: `{"hash": "6400fa014f9e835db82d6f27fb71e100d623eba0ab346fa890304412367e798c", "size_bytes": 310}`,
			want: Digest{
				Hash:      "6400fa014f9e835db82d6f27fb71e100d623eba0ab346fa890304412367e798c",
				SizeBytes: 310,
			},
		},
		{
			name:  "prototext",
			input: `hash: "6400fa014f9e835db82d6f27fb71e100d623eba0ab346fa890304412367e798c" size_bytes: 310`,
			want: Digest{
				Hash:      "6400fa014f9e835db82d6f27fb71e100d623eba0ab346fa890304412367e798c",
				SizeBytes: 310,
			},
		},
		{
			name:  "empty-from-remote-apis-sdks",
			input: "/0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Parse(tc.input)
			if err != nil || d != tc.want {
				t.Errorf("Parse(%q)=%v, %v; want=%v, nil", tc.input, d, err, tc.want)
			}
		})
	}
}
