// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	log "github.com/golang/glog"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"go.chromium.org/build/siso/build"
)

// starFS returns fs module.
//
//	read(fname): reads a file.
//	is_dir(fname): check if fname is a dir.
//	exists(fname): check if fname exists.
//	size(fname): report size of fname's content.
//	canonpath(fname): canonicalize path from working dir relative path.
func starFS(ctx context.Context, fs fs.FS, path *build.Path, fsc *fscache) starlark.Value {
	receiver := starFSReceiver{
		ctx:     ctx,
		fs:      fs,
		path:    path,
		fscache: fsc,
	}
	fsRead := starlark.NewBuiltin("read", starFSRead).BindReceiver(receiver)
	fsIsDir := starlark.NewBuiltin("is_dir", starFSIsDir).BindReceiver(receiver)
	fsExists := starlark.NewBuiltin("exists", starFSExists).BindReceiver(receiver)
	fsSize := starlark.NewBuiltin("size", starFSSize).BindReceiver(receiver)
	fsCanonPath := starlark.NewBuiltin("canonpath", starFSCanonPath).BindReceiver(receiver)
	return starlarkstruct.FromStringDict(starlark.String("fs"), map[string]starlark.Value{
		"read":      fsRead,
		"is_dir":    fsIsDir,
		"exists":    fsExists,
		"size":      fsSize,
		"canonpath": fsCanonPath,
	})
}

type starFSReceiver struct {
	ctx     context.Context
	fs      fs.FS
	path    *build.Path
	fscache *fscache
}

func (r starFSReceiver) String() string {
	return fmt.Sprintf("fs[%s,%s]", r.path.ExecRoot, r.path.Dir)
}

func (starFSReceiver) Type() string          { return "fs" }
func (starFSReceiver) Freeze()               {}
func (starFSReceiver) Truth() starlark.Bool  { return starlark.True }
func (starFSReceiver) Hash() (uint32, error) { return 0, errors.New("fs is not hashable") }

// Starlark function `fs.read(fname)` to return contents of fname.
func starFSRead(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	c, ok := fn.Receiver().(starFSReceiver)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var fname string
	err := starlark.UnpackArgs("read", args, kwargs, "fname", &fname)
	if err != nil {
		return starlark.None, err
	}
	buf, err := c.fscache.Get(c.ctx, c.fs, fname)
	if err != nil {
		return starlark.None, err
	}
	return starlark.Bytes(string(buf)), nil
}

// Starlark function `fs.is_dir(fname)` to check fname is a dir.
func starFSIsDir(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	c, ok := fn.Receiver().(starFSReceiver)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var fname string
	err := starlark.UnpackArgs("is_dir", args, kwargs, "fname", &fname)
	if err != nil {
		return starlark.None, err
	}
	fi, err := fs.Stat(c.fs, fname)
	if err != nil {
		return starlark.None, err
	}
	if fi.IsDir() {
		return starlark.True, nil
	}
	return starlark.False, nil
}

// Starlark function `fs.exists(fname)` to check fname exists.
func starFSExists(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	c, ok := fn.Receiver().(starFSReceiver)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var fname string
	err := starlark.UnpackArgs("exists", args, kwargs, "fname", &fname)
	if err != nil {
		return starlark.None, err
	}
	_, err = fs.Stat(c.fs, fname)
	if err != nil {
		return starlark.False, nil
	}
	return starlark.True, nil
}

// Starlark function `fs.size(fname)` to get file size.
func starFSSize(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	c, ok := fn.Receiver().(starFSReceiver)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var fname string
	err := starlark.UnpackArgs("size", args, kwargs, "fname", &fname)
	if err != nil {
		return starlark.None, err
	}
	fi, err := fs.Stat(c.fs, fname)
	if err != nil {
		return starlark.None, err
	}
	return starlark.MakeInt64(fi.Size()), nil
}

// Starlark function `fs.canonpath(fname)` to canonicalize path from working directory relative path.
func starFSCanonPath(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	log.V(1).Infof("fs.canonpath args=%s kwargs=%s", args, kwargs)
	r, ok := fn.Receiver().(starFSReceiver)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var fname string
	err := starlark.UnpackArgs("canonpath", args, kwargs, "fname", &fname)
	if err != nil {
		return starlark.None, err
	}
	s, err := r.path.FromWD(fname)
	if err != nil {
		return starlark.None, err
	}
	return starlark.String(filepath.ToSlash(s)), nil
}
