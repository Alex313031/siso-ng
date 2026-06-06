// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	_ "embed"
)

// use chromium's build/config/ios/write_framework_hmap.py to
// generate test.hmap data.
// to regenerate, run `CHROMIUM_SRC_DIR=... go generate ./scandeps`.
//go:generate testdata/generate_test_hmap.sh

//go:embed testdata/test.hmap
var testHmapData []byte

func TestParseHeaderMap(t *testing.T) {
	ctx := t.Context()

	got, err := ParseHeaderMap(ctx, testHmapData)
	if err != nil {
		t.Errorf("ParseHeaderMap(ctx, testHmapData)=%v, %v; want nil err", got, err)
	}
	want := map[string]string{
		"Foo/Foo.h": "/private/tmp/siso-scandeps-test/ios/fooFramework/Foo.h",
		"Foo/Bar.h": "/private/tmp/siso-scandeps-test/ios/fooFramework/Bar.h",
		"Foo/Baz.h": "/private/tmp/siso-scandeps-test/ios/fooFramework/Baz.h",
		"Foo.h":     "/private/tmp/siso-scandeps-test/ios/fooFramework/Foo.h",
		"Bar.h":     "/private/tmp/siso-scandeps-test/ios/fooFramework/Bar.h",
		"Baz.h":     "/private/tmp/siso-scandeps-test/ios/fooFramework/Baz.h",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseHeaderMap -want +got:\n%s", diff)
	}
}
