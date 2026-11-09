// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resultstore

import (
	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
)

// Write implements ResultSink.
func (u *Uploader) Write(p []byte) (int, error) {
	u.buildLogMu.Lock()
	defer u.buildLogMu.Unlock()
	return u.buildLog.Write(p)
}

// BuildLogData drains all calls to [Uploader.Write] to CAS store blob.
func (u *Uploader) BuildLogData() blob.Data {
	// The data is uploaded via UploadFiles, which requires HashFS; fall back
	// to SHA-256 when it is not set (the data is then never uploaded).
	fn := digest.SHA256
	if u.hashFS != nil {
		fn = u.hashFS.DigestFunction()
	}
	u.buildLogMu.Lock()
	data := blob.FromBytes(fn, "build.log", u.buildLog.Bytes())
	u.buildLog.Reset()
	u.buildLogMu.Unlock()
	return data
}
