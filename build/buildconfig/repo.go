// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	log "github.com/golang/glog"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"go.chromium.org/build/siso/o11y/clog"
)

const (
	configRepo          = "config"
	configOverridesRepo = "config_overrides"
)

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// repoLoader is a Starlark repository loader.
type repoLoader struct {
	ctx         context.Context
	repos       map[string]fs.FS
	predeclared starlark.StringDict
}

// Load loads a Starlark module.
// A module may be `@<repo>//` prefix to select repository.
func (r *repoLoader) Load(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	curname := thread.Local("modulename").(string)
	log.V(1).Infof("load %s from %s", module, curname)
	var curModule string
	if m, n, ok := strings.Cut(curname, "//"); ok {
		curModule = m[1:]
		curname = n
	}
	curdir := path.Dir(curname)
	var moduleName string
	var fname string
	var fullname string
	if !strings.HasPrefix(module, "@") {
		fname = module
		if !path.IsAbs(fname) {
			fname = path.Join(curdir, module)
		}
		moduleName = curModule
	} else {
		m, n, ok := strings.Cut(module, "//")
		if !ok {
			return nil, fmt.Errorf("failed to parse module: %q", module)
		}
		moduleName = m[1:]
		fname = n
	}
	if moduleName != "" {
		fullname = fmt.Sprintf("@%s//%s", moduleName, fname)
	} else {
		fullname = fname
	}
	log.V(1).Infof("module=%q fname=%q fullname=%q", moduleName, fname, fullname)
	var buf []byte
	var err error
	if moduleName != "" {
		moduleFS, ok := r.repos[moduleName]
		if !ok {
			return nil, fmt.Errorf("no such module defined %q", moduleName)
		}
		buf, err = fs.ReadFile(moduleFS, fname)
	} else {
		buf, err = os.ReadFile(fname)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && moduleName == configOverridesRepo {
			clog.Warningf(r.ctx, "no @%s//%s: %v", configOverridesRepo, fname, err)
			name := strings.TrimSuffix(fname, filepath.Ext(fname))
			return starlark.StringDict(map[string]starlark.Value{
				name: starlark.None,
			}), nil
		}
		return nil, fmt.Errorf("failed to load %s: %w", fullname, err)
	}
	t := &starlark.Thread{
		Name: "module " + module + "(fullname: " + fullname + ")",
		Print: func(thread *starlark.Thread, msg string) {
			log.Infof("thread:%s %s", thread.Name, msg)
		},
		Load: r.Load,
	}
	t.SetLocal("modulename", fullname)
	return starlark.ExecFileOptions(&syntax.FileOptions{Recursion: true}, t, fullname, buf, r.predeclared)
}
