// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resultstore

import (
	"context"
	"runtime"
	"sort"

	rspb "google.golang.org/genproto/googleapis/devtools/resultstore/v2"

	"go.chromium.org/build/siso/reapi/merkletree"
)

// newConfiguration uploads new configuration with properties.
func (u *Uploader) newConfiguration(ctx context.Context, id string, properties map[string]string) error {
	keys := make([]string, 0, len(properties))
	for k := range properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var props Properties
	for _, k := range keys {
		props.Add(k, properties[k])
	}
	req := &rspb.UploadRequest{
		Id: &rspb.UploadRequest_Id{
			ConfigurationId: id,
		},
		UploadOperation: rspb.UploadRequest_CREATE,
		Resource: &rspb.UploadRequest_Configuration{
			Configuration: &rspb.Configuration{
				// StatusAttributes?
				ConfigurationAttributes: &rspb.ConfigurationAttributes{
					Cpu: runtime.GOARCH,
				},
				Properties: []*rspb.Property(props),
			},
		},
	}
	return u.Upload(ctx, req)
}

// UploadBuildConfig uploads default build configuration with properties.
func (u *Uploader) UploadBuildConfig(ctx context.Context, properties map[string]string, ents []merkletree.Entry) error {
	if err := u.newConfiguration(ctx, "default", properties); err != nil {
		return err
	}
	return u.UploadFiles(ctx, ents)
}
