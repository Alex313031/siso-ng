// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"go.chromium.org/build/siso/o11y/clog"
)

// Path manages paths used by the build.
type Path struct {
	WorkspaceRoot string
	BaseDir       string // relative to WorkspaceRoot, use slashes

	// Symbol table for seen paths.
	intern symtab
	// Stores paths converted from out dir relative to workspace relative.
	m sync.Map
}

// NewPath returns new path for the build.
func NewPath(workspaceRoot, baseDir string) *Path {
	return &Path{
		WorkspaceRoot: workspaceRoot,
		BaseDir:       filepath.ToSlash(baseDir),
	}
}

// Check checks the path is valid.
func (p *Path) Check() error {
	if !filepath.IsAbs(p.WorkspaceRoot) {
		return fmt.Errorf("workspace root must be absolute path: %q", p.WorkspaceRoot)
	}
	if filepath.IsAbs(p.BaseDir) {
		return fmt.Errorf("base dir must be relative to workspace: %q", p.BaseDir)
	}
	return nil
}

// Intern interns the path.
func (p *Path) Intern(path string) string {
	return p.intern.Intern(path)
}

// MaybeFromRelative attempts to convert base directory relative to workspace path,
// i.e. workspace root relative path.
// It logs an error and returns the path as-is if this fails.
func (p *Path) MaybeFromRelative(ctx context.Context, path string) string {
	s, err := p.FromRelative(path)
	if err != nil {
		clog.Warningf(ctx, "Failed to get rel %s, %s: %v", p.WorkspaceRoot, path, err)
		return path
	}
	return s
}

// FromRelative converts from base directory relative to workspace path,
// slash-separated.
// It keeps absolute path if it is outside of workspace.
func (p *Path) FromRelative(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	v, ok := p.m.Load(path)
	if ok {
		return v.(string), nil
	}
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(p.WorkspaceRoot, path)
		if err != nil {
			return "", err
		}
		if !filepath.IsLocal(rel) {
			// use abs path for outside of workspace
			return path, nil
		}
		rel = filepath.ToSlash(rel)
		rel = p.intern.Intern(rel)
		v, _ = p.m.LoadOrStore(path, rel)
		return v.(string), nil
	}
	s := filepath.ToSlash(filepath.Join(p.BaseDir, path))
	s = p.intern.Intern(s)
	v, _ = p.m.LoadOrStore(path, s)
	return v.(string), nil
}

// MaybeToRelative converts from workspace path to base directory
// relative, slash-separated. It keeps absolute path as is.
// It logs an error and returns the path as-is if this fails.
func (p *Path) MaybeToRelative(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(p.BaseDir, path)
	if err != nil {
		clog.Warningf(ctx, "Failed to get rel %s, %s: %v", p.BaseDir, path, err)
		return path
	}
	rel = filepath.ToSlash(rel)
	return rel
}

// AbsFromRelative converts base directory relative to absolute path.
func (p *Path) AbsFromRelative(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.WorkspaceRoot, p.BaseDir, path)
}

// AbsBase returns absolute path of base directory.
func (p *Path) AbsBase() string {
	return p.AbsFromRelative(".")
}
