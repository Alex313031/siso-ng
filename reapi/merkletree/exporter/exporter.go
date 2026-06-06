// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package exporter is an exporter of directory tree from RBE-CAS.
package exporter

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/sync/semaphore"
)

// Client is an interface to access CAS.
type Client interface {
	Get(context.Context, digest.Digest, string) ([]byte, error)
	FetchTree(context.Context, string, digest.Digest, *digest.Store) (*rpb.Directory, error)
}

// Exporter is an exporter.
type Exporter struct {
	client Client
	eg     errgroup.Group
	sema   *semaphore.Semaphore
}

// New creates new exporter.
func New(client Client) *Exporter {
	return &Exporter{
		client: client,
		sema:   semaphore.New("exporter", runtime.GOMAXPROCS(0)),
	}
}

// Export exports directory identified by the digest to the dir recursively.
// If w is given, it will show the directory entries without extracting
// into dir.
func (e *Exporter) Export(ctx context.Context, dir string, d digest.Digest, w io.Writer) error {
	e.eg.Go(func() error {
		return e.sema.Do(ctx, func(ctx context.Context) error {
			return e.exportDir(ctx, dir, d, w)
		})
	})
	return e.eg.Wait()
}

func (e *Exporter) exportDir(ctx context.Context, dir string, d digest.Digest, w io.Writer) error {
	clog.Infof(ctx, "export dir: %s %s", dir, d)
	if w == nil {
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			return err
		}
	} else if dir != "." && dir != "" {
		fmt.Fprintf(w, "%s\t%s\tdirectory\n", dir, d)
	}
	b, err := e.client.Get(ctx, d, dir)
	if err != nil {
		return err
	}
	curdir := &rpb.Directory{}
	err = proto.Unmarshal(b, curdir)
	if err != nil {
		return fmt.Errorf("failed to unmarshal dir for %s from %s: %w", dir, d, err)
	}
	for _, f := range curdir.Files {
		e.eg.Go(func() error {
			return e.sema.Do(ctx, func(ctx context.Context) error {
				return e.exportFile(ctx, filepath.Join(dir, f.Name), digest.FromProto(f.Digest), f.IsExecutable, w)
			})
		})
	}
	for _, subdir := range curdir.Directories {
		e.eg.Go(func() error {
			return e.sema.Do(ctx, func(ctx context.Context) error {
				return e.exportDir(ctx, filepath.Join(dir, subdir.Name), digest.FromProto(subdir.Digest), w)
			})
		})
	}
	for _, s := range curdir.Symlinks {
		fname := filepath.Join(dir, s.Name)
		clog.Infof(ctx, "symlink %s -> %s", fname, s.Target)
		if w == nil {
			err := os.Symlink(s.Target, fname)
			if err != nil {
				return err
			}
		} else {
			fmt.Fprintf(w, "%s\t-> %s\n", fname, s.Target)
		}
	}
	return nil
}

func (e *Exporter) exportFile(ctx context.Context, fname string, d digest.Digest, isExecutable bool, w io.Writer) error {
	clog.Infof(ctx, "file:%s %s x:%t", fname, d, isExecutable)
	if w != nil {
		if isExecutable {
			fmt.Fprintf(w, "%s\t%s\texecutable\n", fname, d)
		} else {
			fmt.Fprintf(w, "%s\t%s\tfile\n", fname, d)
		}
		return nil
	}
	b, err := e.client.Get(ctx, d, fname)
	if err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if isExecutable {
		mode = os.FileMode(0755)
	}
	err = os.WriteFile(fname, b, mode)
	if err != nil {
		return err
	}
	return os.Chmod(fname, mode)
}

// ExportTree exports tree identified by the digest to the dir recursively.
// If w is given, it will show the directory entries without extracting
// into dir.
func (e *Exporter) ExportTree(ctx context.Context, dir string, d digest.Digest, w io.Writer) error {
	ds := digest.NewStore()
	root, err := e.client.FetchTree(ctx, dir, d, ds)
	if err != nil {
		return err
	}
	files, symlinks, dirs := merkletree.Traverse(ctx, dir, root, ds)
	if w == nil {
		for _, file := range files {
			e.eg.Go(func() error {
				return e.sema.Do(ctx, func(ctx context.Context) error {
					err := os.MkdirAll(filepath.Dir(file.GetPath()), 0755)
					if err != nil {
						return err
					}
					mode := fs.FileMode(0644)
					if file.GetIsExecutable() {
						mode = fs.FileMode(0755)
					}
					b, err := e.client.Get(ctx, digest.FromProto(file.GetDigest()), file.GetPath())
					if err != nil {
						return fmt.Errorf("error from client.Get for %s: %w", file.GetPath(), err)
					}
					return os.WriteFile(file.GetPath(), b, mode)
				})
			})
		}
		for _, symlink := range symlinks {
			e.eg.Go(func() error {
				return e.sema.Do(ctx, func(ctx context.Context) error {
					err := os.MkdirAll(filepath.Dir(symlink.GetPath()), 0755)
					if err != nil {
						return err
					}
					return os.Symlink(symlink.GetTarget(), symlink.GetPath())
				})
			})
		}
		for _, dir := range dirs {
			e.eg.Go(func() error {
				return e.sema.Do(ctx, func(ctx context.Context) error {
					return os.MkdirAll(dir.GetPath(), 0755)
				})
			})
		}
		return e.eg.Wait()
	} else {
		for _, file := range files {
			if file.GetIsExecutable() {
				fmt.Fprintf(w, "%s\t%s\texecutable\n", file.GetPath(), digest.FromProto(file.GetDigest()))
			} else {
				fmt.Fprintf(w, "%s\t%s\tfile\n", file.GetPath(), digest.FromProto(file.GetDigest()))
			}
		}
		for _, symlink := range symlinks {
			fmt.Fprintf(w, "%s\t-> %s\n", symlink.GetPath(), symlink.GetTarget())
		}
		for _, dir := range dirs {
			fmt.Fprintf(w, "%s\t%s\tdirectory\n", dir.GetPath(), digest.FromProto(dir.GetTreeDigest()))
		}
	}
	return nil
}
