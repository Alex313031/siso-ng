// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resultstore

import (
	"go.chromium.org/build/siso/reapi/digest"
)

func (u *Uploader) AddBuildLog(msg string) {
	u.buildLogMu.Lock()
	u.buildLog.WriteString(msg)
	u.buildLogMu.Unlock()
}

func (u *Uploader) BuildLogData() digest.Data {
	u.buildLogMu.Lock()
	data := digest.FromBytes("build.log", u.buildLog.Bytes())
	u.buildLog.Reset()
	u.buildLogMu.Unlock()
	return data
}
