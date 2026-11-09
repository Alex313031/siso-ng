// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/path"
)

// DirChild is one expected direct child of a directory, for MatchDir.
type DirChild struct {
	// Name is the base name.
	Name string
	// Digest is the expected content of a file. Zero for a directory.
	Digest digest.Digest
	// IsExecutable is the expected executable bit of a file.
	IsExecutable bool
}

// MatchDir reports whether every child in want is recorded directly under
// the directory fulldir, a full path as returned by MakeFullpath: a file
// with exactly the given digest and executable bit, or a directory.
//
// It is a fast path for callers that would otherwise call Entries for the
// same children and compare. It only reads what is already in memory: it
// never reads the disk, computes a digest, blocks or allocates, and it keeps
// no state between calls. False means "not known to match", not "mismatch":
// the caller must then fall back to Entries, which gives the authoritative
// answer and loads what was missing. In particular it returns false when
// fulldir is reached through a symlink, when a child is a symlink, is not
// loaded yet, has an error, or has no digest yet.
//
// True implies that Entries on the same children, at the same moment, would
// return a file entry with the same digest and executable bit for every file
// and a directory entry for every directory.
func (hfs *HashFS) MatchDir(ctx context.Context, fulldir string, want []DirChild) bool {
	e, fname, _, ok := hfs.directory.lookup(ctx, path.Path(fulldir))
	if !ok || e == nil || string(fname) != fulldir {
		// Not recorded, or resolved through a symlink.
		return false
	}
	// Only a file's digest computation writes e.err after e is published
	// (under e.mu). A directory has no src, so its err is fixed.
	d := e.getDir()
	if d == nil || e.err != nil || e.isSymlink() {
		return false
	}
	for _, w := range want {
		v, ok := d.m.Load(w.Name)
		if !ok {
			return false
		}
		ce := v.(*entry)
		if ce.isSymlink() {
			return false
		}
		ce.mu.RLock()
		err, dg, mode := ce.err, ce.d, ce.mode
		ce.mu.RUnlock()
		if err != nil {
			return false
		}
		if w.Digest.IsZero() {
			// Want a directory.
			if !ce.isDirectory() || !dg.IsZero() {
				return false
			}
			continue
		}
		if ce.isDirectory() || dg != w.Digest || (mode&0111 != 0) != w.IsExecutable {
			return false
		}
	}
	return true
}
