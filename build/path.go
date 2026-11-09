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
	"go.chromium.org/build/siso/path"
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

// Intern interns the path string.
func (p *Path) Intern(s string) string {
	return p.intern.Intern(s)
}

// InternPath interns the path, returning a path.Path that shares storage
// with other equal paths seen during this build.
func (p *Path) InternPath(pp path.Path) path.Path {
	return path.Path(p.intern.Intern(string(pp)))
}

// MaybeFromRelative attempts to convert base directory relative to workspace path,
// i.e. workspace root relative path.
// It logs an error and returns the path as-is if this fails.
func (p *Path) MaybeFromRelative(ctx context.Context, s string) string {
	pp, err := p.FromRelative(s)
	if err != nil {
		clog.Warningf(ctx, "Failed to get rel %s, %s: %v", p.WorkspaceRoot, s, err)
		return s
	}
	return pp
}

// FromRelativePath is MaybeFromRelative returning a path.Path. Separators are
// normalized to "/", but ".." is not collapsed: an absolute path outside the
// workspace may have ".." crossing a symlink that only the OS can resolve.
func (p *Path) FromRelativePath(ctx context.Context, s string) path.Path {
	// filepath.ToSlash converts native separators (a no-op on POSIX) without
	// lexically cleaning "..", unlike path.New.
	return path.FromClean(filepath.ToSlash(p.MaybeFromRelative(ctx, s)))
}

// FromRelative converts from base directory relative to workspace path,
// slash-separated.
// It keeps absolute path if it is outside of workspace.
func (p *Path) FromRelative(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	v, ok := p.m.Load(s)
	if ok {
		return v.(string), nil
	}
	if filepath.IsAbs(s) {
		rel, err := filepath.Rel(p.WorkspaceRoot, s)
		if err != nil {
			return "", err
		}
		if !filepath.IsLocal(rel) {
			// Absolute paths outside the workspace are returned
			// byte-identical: callers hand them to tools and OS APIs.
			return s, nil
		}
		pp := p.intern.Intern(filepath.ToSlash(rel))
		v, _ = p.m.LoadOrStore(s, pp)
		return v.(string), nil
	}
	pp := p.intern.Intern(filepath.ToSlash(filepath.Join(p.BaseDir, s)))
	v, _ = p.m.LoadOrStore(s, pp)
	return v.(string), nil
}

// MaybeToRelative converts from workspace path to base directory
// relative, slash-separated. It keeps absolute path as is.
// It logs an error and returns the path as-is if this fails.
func (p *Path) MaybeToRelative(ctx context.Context, s string) string {
	if s == "" {
		return ""
	}
	if filepath.IsAbs(s) {
		return s
	}
	rel, err := filepath.Rel(p.BaseDir, s)
	if err != nil {
		clog.Warningf(ctx, "Failed to get rel %s, %s: %v", p.BaseDir, s, err)
		return s
	}
	// filepath.Rel returns OS-native separators; the contract here is
	// slash-separated.
	return filepath.ToSlash(rel)
}

// AbsFromRelative converts base directory relative to absolute path.
func (p *Path) AbsFromRelative(s string) string {
	if filepath.IsAbs(s) {
		return s
	}
	return filepath.Join(p.WorkspaceRoot, p.BaseDir, s)
}

// AbsBase returns absolute path of base directory.
func (p *Path) AbsBase() string {
	return p.AbsFromRelative(".")
}
