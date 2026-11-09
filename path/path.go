// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package path provides a normalized, forward-slash-separated path type.
//
// Path values are immutable and guaranteed to be clean (no ".", "..",
// duplicate slashes, or trailing slashes). On Windows, backslashes are
// converted to forward slashes at construction time.
//
// This package intentionally shadows the standard library "path" package.
// Siso uses "path/filepath" for OS-native path operations; this package
// replaces the ad-hoc filepath.ToSlash + filepath.Join patterns used for
// build-relative paths throughout the codebase.
//
// JoinRoot memoizes results in a process-global cache; siso runs one
// build per process, so the cache is never reset.
package path

import (
	"hash/maphash"
	stdpath "path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// joinCacheShards is the shard count of the JoinRoot cache. It is a power
// of two so a rel hash selects its shard with a bit mask.
const (
	joinCacheShards = 64
	joinCacheMask   = joinCacheShards - 1
)

type joinKey struct {
	root, rel string
}

// joinShard is one shard of the JoinRoot cache: an RWMutex-protected
// plain map. Sharded maps keep warm reads contention-free without
// sync.Map's atomic overhead.
type joinShard struct {
	mu sync.RWMutex
	m  map[joinKey]Path
	// roots interns root key strings: one build uses one workspace root.
	roots map[string]string
}

var (
	joinShards   [joinCacheShards]joinShard
	joinHashSeed = maphash.MakeSeed()
)

func joinShardFor(rel string) *joinShard {
	return &joinShards[maphash.String(joinHashSeed, rel)&joinCacheMask]
}

// Path is a forward-slash-separated, cleaned path string.
//
// It is either relative (e.g. "out/Debug/obj/foo.o") or absolute with
// forward slashes (e.g. "/usr/bin/clang"). The zero value is the empty
// path "".
//
// Path is a defined type over string: it can be used as a map key,
// compared with == and <, and converted to/from string at zero cost.
type Path string

// New normalizes s into a Path: path.Clean(filepath.ToSlash(s)) with
// the UNC root preserved, allocation-free when s is already clean.
func New(s string) Path {
	if s == "" {
		return ""
	}
	// On non-Windows, backslashes are not path separators.
	// Skip the ToSlash conversion (the common case).
	if runtime.GOOS != "windows" {
		return cleanSlashPath(s)
	}
	return cleanSlashPath(filepath.ToSlash(s))
}

// isUNCRoot reports whether s begins with the UNC double-slash root
// (//server/share); three or more leading slashes are not a UNC root.
func isUNCRoot(s string) bool {
	return len(s) >= 2 && s[0] == '/' && s[1] == '/' && (len(s) == 2 || s[2] != '/')
}

// cleanSlashPath cleans a forward-slash path, preserving a UNC
// double-slash root (//server/share) that path.Clean would collapse.
func cleanSlashPath(s string) Path {
	cleaned := stdpath.Clean(s)
	if isUNCRoot(s) && !isUNCRoot(cleaned) {
		return Path("/" + cleaned)
	}
	return Path(cleaned)
}

// Equal reports whether two Paths are the same after canonicalization,
// treating "" and "." as the same (root) path.
func (p Path) Equal(other Path) bool {
	norm := func(x Path) Path {
		c := New(string(x))
		if c == "." {
			return ""
		}
		return c
	}
	return norm(p) == norm(other)
}

// FromClean converts a string that is already normalized (forward-slash,
// cleaned) into a Path without re-cleaning. Use this instead of a bare
// Path(s) cast to make intentional unchecked conversions grep-able.
//
// The caller must ensure s is already a valid clean path. If unsure,
// use New(s) instead.
func FromClean(s string) Path {
	return Path(s)
}

// Join returns p/elem, cleaned.
func (p Path) Join(elem string) Path {
	if p == "" {
		return New(elem)
	}
	if elem == "" {
		return p
	}
	if p == "/" {
		// Do not fabricate a leading "//": it reads as a UNC root.
		return New("/" + elem)
	}
	return New(string(p) + "/" + elem)
}

// JoinPath returns p/elem.
// More efficient than Join when elem is already a Path.
func (p Path) JoinPath(elem Path) Path {
	if p == "" {
		return elem
	}
	if elem == "" {
		return p
	}
	// Both sides are clean, so p + "/" + elem is clean unless the
	// boundary introduces an artifact (".." collapse, "//" seam, root
	// "/"); only those cases go through Clean.
	if p == "/" {
		// Do not fabricate a leading "//": it reads as a UNC root.
		return cleanSlashPath("/" + string(elem))
	}
	s := string(p) + "/" + string(elem)
	if elem[0] == '.' || elem[0] == '/' {
		return cleanSlashPath(s)
	}
	return Path(s)
}

// Dir returns the directory component of the path (everything before the
// last slash). Returns "." if there is no slash.
//
// Operates on forward-slash paths only; does not consult the OS. Roots
// are their own parent (Dir("/") = "/", Dir("C:/") = "C:/"; UNC paths
// keep their double-slash root), so parent walks terminate.
func (p Path) Dir() Path {
	s := string(p)
	if isUNCRoot(s) {
		// Walk the sub-path so the shared root's double slash is not
		// collapsed by Clean inside stdpath.Dir.
		return Path("/" + stdpath.Dir(s[1:]))
	}
	d := stdpath.Dir(s)
	// stdpath treats a drive prefix as an ordinary element and would
	// walk C:/src -> C: -> "."; stop at the drive root instead.
	if len(d) == 2 && d[1] == ':' && isDriveLetter(d[0]) {
		return Path(d + "/")
	}
	return Path(d)
}

// Base returns the last element of the path.
// Returns "." if the path is empty.
func (p Path) Base() Path {
	return Path(stdpath.Base(string(p)))
}

// Ext returns the file extension: the suffix beginning at the last dot
// in the last element of the path.
func (p Path) Ext() string {
	return stdpath.Ext(string(p))
}

// HasPrefix reports whether p starts with prefix at a segment boundary.
//
// prefix must itself be a clean Path, so a trailing-slash prefix such as
// "foo/" never matches: a clean Path has no trailing slash.
//
//	HasPrefix("foo/bar", "foo")    = true
//	HasPrefix("foobar", "foo")    = false
//	HasPrefix("foo/bar", "foo/")  = false
//	HasPrefix("foo", "foo")       = true
//	HasPrefix("foo", "")          = true
func (p Path) HasPrefix(prefix Path) bool {
	if prefix == "" {
		return true
	}
	s := string(p)
	pre := string(prefix)
	if !strings.HasPrefix(s, pre) {
		return false
	}
	return len(s) == len(pre) || s[len(pre)] == '/'
}

// TrimPrefix removes prefix from p at a segment boundary, returning the
// remainder without a leading slash. Returns p unchanged if p does not
// start with prefix.
//
//	TrimPrefix("foo/bar/baz", "foo")     = "bar/baz"
//	TrimPrefix("foo/bar/baz", "foo/bar") = "baz"
//	TrimPrefix("foo", "foo")             = ""
//	TrimPrefix("foobar", "foo")          = "foobar"
func (p Path) TrimPrefix(prefix Path) Path {
	if prefix == "" || !p.HasPrefix(prefix) {
		return p
	}
	s := string(p)
	pre := string(prefix)
	if len(s) == len(pre) {
		return ""
	}
	// Skip the separator after the prefix.
	return Path(s[len(pre)+1:])
}

// Rel returns a relative path from base to p.
// Both must be relative, or both must be absolute.
func (p Path) Rel(base Path) (Path, error) {
	// Use filepath.Rel for correctness (handles ".." traversal),
	// then normalize back to forward slashes.
	rel, err := filepath.Rel(string(base), string(p))
	if err != nil {
		return "", err
	}
	return New(rel), nil
}

// String returns the underlying path string.
func (p Path) String() string {
	return string(p)
}

// IsAbs reports whether the path is absolute: it begins with '/' or is
// Windows drive-absolute ("C:/foo"). The drive form is recognized on
// every host so paths from Windows depfiles classify consistently.
// A bare drive ("C:"), a drive-relative path ("C:foo"), and a mid-path
// ':' (ninja label "foo:bar") are all relative.
func (p Path) IsAbs() bool {
	s := string(p)
	if len(s) > 0 && s[0] == '/' {
		return true
	}
	// Windows drive-absolute: "X:/...".
	return len(s) >= 3 && isDriveLetter(s[0]) && s[1] == ':' && s[2] == '/'
}

// isDriveLetter reports whether c is an ASCII letter usable as a
// Windows drive designator.
func isDriveLetter(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z'
}

// IsEmpty reports whether the path is the zero value "".
func (p Path) IsEmpty() bool {
	return p == ""
}

// OSPath converts the path to an OS-native representation.
// On Unix this returns the path unchanged. On Windows it converts
// forward slashes to backslashes.
func (p Path) OSPath() string {
	if runtime.GOOS != "windows" {
		return string(p)
	}
	return filepath.FromSlash(string(p))
}

// JoinRoot joins an OS-native root directory with a slash-separated
// relative path into a clean Path, replacing the common
// filepath.ToSlash(filepath.Join(root, fname)) pattern. rel may carry
// un-cleaned segments from untrusted sources; JoinRoot normalizes them
// so downstream lookups match.
//
// Results are memoized: a build calls JoinRoot many times over a small
// set of distinct (root, rel) pairs.
func JoinRoot(root string, rel Path) Path {
	if rel.IsAbs() {
		return rel
	}
	if root == "" {
		return New(string(rel))
	}
	if rel == "" {
		return New(root)
	}
	if rel == "." {
		return New(root)
	}
	k := joinKey{root, string(rel)}
	s := joinShardFor(k.rel)
	s.mu.RLock()
	if p, ok := s.m[k]; ok {
		s.mu.RUnlock()
		return p
	}
	s.mu.RUnlock()
	var p Path
	if runtime.GOOS != "windows" {
		r := root
		var str string
		if r[len(r)-1] == '/' {
			str = r + string(rel)
		} else {
			str = r + "/" + string(rel)
		}
		p = cleanSlashPath(str)
	} else {
		p = New(filepath.Join(root, string(rel)))
	}
	s.mu.Lock()
	if existing, ok := s.m[k]; ok {
		s.mu.Unlock()
		return existing
	}
	if s.m == nil {
		// Allocated lazily; reading a nil map on the hit path is safe.
		s.m = make(map[joinKey]Path)
		s.roots = make(map[string]string)
	}
	// Clone the key strings so a cache entry never pins a larger buffer
	// they may be sliced from; root repeats across entries, so intern it
	// per shard instead of cloning each time.
	iroot, ok := s.roots[k.root]
	if !ok {
		iroot = strings.Clone(k.root)
		s.roots[iroot] = iroot
	}
	s.m[joinKey{iroot, strings.Clone(k.rel)}] = p
	s.mu.Unlock()
	return p
}

// Strings converts a slice of Paths to a slice of strings.
// The string values are shared, not copied.
func Strings(ps []Path) []string {
	ss := make([]string, len(ps))
	for i, p := range ps {
		ss[i] = string(p)
	}
	return ss
}

// Paths converts a slice of already-normalized strings to a slice of Paths.
// No normalization is performed. Use NewPaths for untrusted input.
func Paths(ss []string) []Path {
	ps := make([]Path, len(ss))
	for i, s := range ss {
		ps[i] = Path(s)
	}
	return ps
}
