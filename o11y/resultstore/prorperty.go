// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resultstore

import (
	rspb "google.golang.org/genproto/googleapis/devtools/resultstore/v2"
)

// Properties are invocation properties.
type Properties []*rspb.Property

// Add adds new key/value pair.
func (p *Properties) Add(key, value string) {
	*p = append(*p, &rspb.Property{
		Key:   key,
		Value: value,
	})
}
