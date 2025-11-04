// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"fmt"
	"path/filepath"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// starPath returns path module.
//
//	base(fname)
//	dir(fname)
//	join(...)
//	rel(basepath, targetpath)
//	isabs(fname)
func starPath() starlark.Value {
	pathModule := &starlarkstruct.Module{
		Name: "path",
		Members: map[string]starlark.Value{
			"base":  starlark.NewBuiltin("base", starPathBase),
			"dir":   starlark.NewBuiltin("dir", starPathDir),
			"join":  starlark.NewBuiltin("join", starPathJoin),
			"rel":   starlark.NewBuiltin("rel", starPathRel),
			"isabs": starlark.NewBuiltin("isabs", starPathIsAbs),
		},
	}
	pathModule.Freeze()
	return pathModule
}

// Starlark function `path.base(fname)` to return base name of fname.
func starPathBase(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var fname string
	err := starlark.UnpackArgs("base", args, kwargs, "fname", &fname)
	if err != nil {
		return starlark.None, err
	}
	return starlark.String(filepath.Base(fname)), nil
}

// Starlark function `path.dir(fname)` to return dir name of fname.
func starPathDir(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var fname string
	err := starlark.UnpackArgs("dir", args, kwargs, "fname", &fname)
	if err != nil {
		return starlark.None, err
	}
	return starlark.String(filepath.ToSlash(filepath.Dir(fname))), nil
}

// Starlark function `path.join(...)` to return joined path name.
func starPathJoin(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var elems []string
	for _, v := range args {
		s, ok := starlark.AsString(v)
		if !ok {
			return starlark.None, fmt.Errorf("join: for parameter elems: got %s, want string", v.Type())
		}
		elems = append(elems, s)
	}
	return starlark.String(filepath.ToSlash(filepath.Join(elems...))), nil
}

// Starlark function `path.rel(basepath, targetpath)` to return relative path of targetpath from basepath.
func starPathRel(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var basepath, targetpath string
	err := starlark.UnpackArgs("rel", args, kwargs, "basepath", &basepath, "targetpath", &targetpath)
	if err != nil {
		return starlark.None, err
	}
	rel, err := filepath.Rel(basepath, targetpath)
	if err != nil {
		return starlark.None, err
	}
	return starlark.String(filepath.ToSlash(rel)), nil
}

// Starlark function `path.isabs(fname)` to return true if fname is absolute path.
func starPathIsAbs(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var fname string
	err := starlark.UnpackArgs("isabs", args, kwargs, "fname", &fname)
	if err != nil {
		return starlark.None, err
	}
	// Should return true for /path or \path on Windows?
	r := filepath.IsAbs(fname)
	return starlark.Bool(r), nil
}
