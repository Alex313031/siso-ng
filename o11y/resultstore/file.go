// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resultstore

import (
	"context"
	"fmt"

	rspb "google.golang.org/genproto/googleapis/devtools/resultstore/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// hashType maps a digest function to the ResultStore file hash type.
// ResultStore only models MD5/SHA1/SHA256; anything else is unspecified.
func hashType(fn digest.Function) rspb.File_HashType {
	switch fn.Value() {
	case rpb.DigestFunction_SHA256:
		return rspb.File_SHA256
	case rpb.DigestFunction_SHA1:
		return rspb.File_SHA1
	case rpb.DigestFunction_MD5:
		return rspb.File_MD5
	default:
		return rspb.File_HASH_TYPE_UNSPECIFIED
	}
}

// digestFunction returns the digest function that produced the digests this
// uploader attaches to files: HashFS's function, falling back to the REAPI
// client's, then SHA-256.
func (u *Uploader) digestFunction() digest.Function {
	if u.hashFS != nil {
		return u.hashFS.DigestFunction()
	}
	if u.reapiClient != nil {
		return u.reapiClient.DigestFunction()
	}
	return digest.SHA256
}

// UploadFiles uploads files to RBE-CAS, and sets the files as the invocation's artifact.
// Need to set HashFS, REAPIClient to Options before calling this.
func (u *Uploader) UploadFiles(ctx context.Context, ents []merkletree.Entry) error {
	if u.hashFS == nil || u.reapiClient == nil {
		return fmt.Errorf("resultstore: unable to upload file. hashfs or reapi client is not set")
	}
	ds := blob.NewStore()
	ht := hashType(u.digestFunction())
	var files []*rspb.File
	for _, ent := range ents {
		file := &rspb.File{
			Uid: string(ent.Name),
		}
		if ent.Data.IsZero() {
			continue
		}
		ds.Set(ent.Data)
		d := ent.Data.Digest()
		file.Uri = u.reapiClient.FileURI(d)
		file.Length = &wrapperspb.Int64Value{
			Value: d.SizeBytes,
		}
		// file.ContentType ?
		file.Digest = d.Hash
		file.HashType = ht
		files = append(files, file)
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case u.q <- ds:
	}
	req := &rspb.UploadRequest{
		UploadOperation: rspb.UploadRequest_MERGE,
		UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{
				"files",
			},
		},
		Resource: &rspb.UploadRequest_Invocation{
			Invocation: &rspb.Invocation{
				Files: files,
			},
		},
	}
	return u.Upload(ctx, req)
}

// uploadFile set a file as the invocation's artifact.
func (u *Uploader) uploadFile(ctx context.Context, name string, d digest.Digest) error {
	var uri string
	if u.reapiClient != nil {
		uri = u.reapiClient.FileURI(d)
	}
	req := &rspb.UploadRequest{
		UploadOperation: rspb.UploadRequest_MERGE,
		UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{
				"files",
			},
		},
		Resource: &rspb.UploadRequest_Invocation{
			Invocation: &rspb.Invocation{
				Files: []*rspb.File{
					{
						Uid: name,
						Uri: uri,
						Length: &wrapperspb.Int64Value{
							Value: d.SizeBytes,
						},
						Digest:   d.Hash,
						HashType: hashType(u.digestFunction()),
					},
				},
			},
		},
	}
	return u.Upload(ctx, req)
}

// UploadBuildNinja implements build.ResultSink.
func (u *Uploader) UploadBuildNinja(ctx context.Context, d digest.Digest) error {
	return u.uploadFile(ctx, "build.ninja.dir", d)
}
