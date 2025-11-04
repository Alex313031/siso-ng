// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package merkletree

import (
	"context"
	"path/filepath"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/digest"
)

// Traverse traverses a directory recursively, and returns the found files, symlinks and directories.
// The base directory name is prepended to each path of the entries.
// The directories not registered in the digest.Store will be ignored.
func Traverse(ctx context.Context, base string, dir *rpb.Directory, ds *digest.Store) ([]*rpb.OutputFile, []*rpb.OutputSymlink, []*rpb.OutputDirectory) {
	var files []*rpb.OutputFile
	for _, f := range dir.Files {
		files = append(files, &rpb.OutputFile{
			Path:         filepath.Join(base, f.Name),
			Digest:       f.Digest,
			IsExecutable: f.IsExecutable,
		})
	}

	var symlinks []*rpb.OutputSymlink
	for _, s := range dir.Symlinks {
		symlinks = append(symlinks, &rpb.OutputSymlink{
			Path:   filepath.Join(base, s.Name),
			Target: s.Target,
		})
	}

	var dirs []*rpb.OutputDirectory
	for _, subd := range dir.Directories {
		subdirname := filepath.Join(base, subd.Name)
		dg := digest.FromProto(subd.Digest)
		db, found := ds.Get(dg)
		if !found {
			// TODO(b/269199873): revisit error handling.
			clog.Errorf(ctx, "digest.Store doesn't have a directory: %s %s", subdirname, dg)
			continue
		}
		subdir := &rpb.Directory{}
		err := readProto(ctx, db, subdir)
		if err != nil {
			// TODO(b/269199873): revisit error handling.
			clog.Errorf(ctx, "invalid rpb.Directory proto:%s %s", subdirname, dg)
			continue
		}
		dirs = append(dirs, &rpb.OutputDirectory{
			Path:       filepath.Join(base, subd.Name),
			TreeDigest: digest.Empty.Proto(),
		})
		sfiles, ssymlinks, sdirs := Traverse(ctx, subdirname, subdir, ds)
		files = append(files, sfiles...)
		symlinks = append(symlinks, ssymlinks...)
		dirs = append(dirs, sdirs...)
	}
	return files, symlinks, dirs
}

func readProto(ctx context.Context, data digest.Data, m proto.Message) error {
	b, err := digest.DataToBytes(ctx, data)
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, m)
}
