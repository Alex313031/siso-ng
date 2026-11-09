// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package path

import (
	"fmt"
	"sync"
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		in   string
		want Path
	}{
		{"", ""},
		{".", "."},
		{"foo", "foo"},
		{"foo/bar", "foo/bar"},
		{"foo/bar/", "foo/bar"},
		{"foo//bar", "foo/bar"},
		{"foo/./bar", "foo/bar"},
		{"foo/bar/../baz", "foo/baz"},
		{"/absolute/path", "/absolute/path"},
		{"/absolute/path/", "/absolute/path"},
		{"a/b/../c/./d", "a/c/d"},
		{"../parent", "../parent"},
		{"./relative", "relative"},
		// A UNC root keeps its double slash; three or more leading
		// slashes are not a UNC root and collapse.
		{"//server/share/x", "//server/share/x"},
		{"//server/share/a/../b", "//server/share/b"},
		{"///server/share", "/server/share"},
	}
	for _, tt := range tests {
		got := New(tt.in)
		if got != tt.want {
			t.Errorf("New(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNewIdempotent(t *testing.T) {
	paths := []string{
		"foo/bar",
		"out/Debug/obj/base/foo.o",
		"/usr/bin/clang",
		".",
		"",
	}
	for _, s := range paths {
		p := New(s)
		p2 := New(string(p))
		if p != p2 {
			t.Errorf("New(New(%q)) = %q, want %q", s, p2, p)
		}
	}
}

func TestPathEqual(t *testing.T) {
	tests := []struct {
		p1, p2 Path
		want   bool
	}{
		{"", "", true},
		{".", "", true},
		{".", ".", true},
		{"out", "out", true},
		{"out/siso", "out/siso/", true},
		{"out", "out/siso", false},
		{".", "out", false},
		{"", "out", false},
	}
	for _, tt := range tests {
		got := tt.p1.Equal(tt.p2)
		if got != tt.want {
			t.Errorf("(%q).Equal(%q) = %t, want %t", tt.p1, tt.p2, got, tt.want)
		}
	}
}

func TestJoin(t *testing.T) {
	tests := []struct {
		base Path
		elem string
		want Path
	}{
		{"", "foo", "foo"},
		{"foo", "", "foo"},
		{"", "", ""},
		{"foo", "bar", "foo/bar"},
		{"foo/bar", "baz", "foo/bar/baz"},
		{"foo", "bar/baz", "foo/bar/baz"},
		{"foo", "../bar", "bar"},
		{"foo/bar", "../baz", "foo/baz"},
	}
	for _, tt := range tests {
		got := tt.base.Join(tt.elem)
		if got != tt.want {
			t.Errorf("%q.Join(%q) = %q, want %q", tt.base, tt.elem, got, tt.want)
		}
	}
}

func TestJoinPath(t *testing.T) {
	tests := []struct {
		base Path
		elem Path
		want Path
	}{
		{"", "foo", "foo"},
		{"foo", "", "foo"},
		{"", "", ""},
		{"foo", "bar", "foo/bar"},
		{"foo/bar", "baz/qux", "foo/bar/baz/qux"},
		{"foo", "../bar", "bar"},
		{"foo/bar", "../baz", "foo/baz"},
		// The fast path must not emit an unclean Path. An elem with a
		// leading slash would otherwise produce "foo//abs".
		{"foo", "/abs", "foo/abs"},
		// A root "/" base would otherwise produce "//usr".
		{"/", "usr", "/usr"},
	}
	for _, tt := range tests {
		got := tt.base.JoinPath(tt.elem)
		if got != tt.want {
			t.Errorf("%q.JoinPath(%q) = %q, want %q", tt.base, tt.elem, got, tt.want)
		}
	}
}

func TestDir(t *testing.T) {
	tests := []struct {
		in   Path
		want Path
	}{
		{"foo/bar", "foo"},
		{"foo/bar/baz", "foo/bar"},
		{"foo", "."},
		{".", "."},
		{"", "."},
		{"/foo/bar", "/foo"},
		{"/foo", "/"},
		// A drive-absolute path never walks above its drive root, so
		// parent walks that stop at a trailing slash terminate.
		{"C:/src/out", "C:/src"},
		{"C:/src", "C:/"},
		{"C:/", "C:/"},
		// A UNC path keeps its double-slash root.
		{"//server/share/x", "//server/share"},
	}
	for _, tt := range tests {
		got := tt.in.Dir()
		if got != tt.want {
			t.Errorf("%q.Dir() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBase(t *testing.T) {
	tests := []struct {
		in   Path
		want Path
	}{
		{"foo/bar", "bar"},
		{"foo/bar/baz.o", "baz.o"},
		{"foo", "foo"},
		{".", "."},
		{"", "."},
		{"/", "/"},
	}
	for _, tt := range tests {
		got := tt.in.Base()
		if got != tt.want {
			t.Errorf("%q.Base() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExt(t *testing.T) {
	tests := []struct {
		in   Path
		want string
	}{
		{"foo.go", ".go"},
		{"foo/bar.tar.gz", ".gz"},
		{"foo/bar", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := tt.in.Ext()
		if got != tt.want {
			t.Errorf("%q.Ext() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHasPrefix(t *testing.T) {
	tests := []struct {
		path   Path
		prefix Path
		want   bool
	}{
		{"foo/bar", "foo", true},
		{"foo/bar/baz", "foo/bar", true},
		{"foo", "foo", true},
		{"foobar", "foo", false},
		// A prefix is a clean Path and never has a trailing slash, so a
		// trailing-slash prefix does not match at a segment boundary.
		{"foo/bar", "foo/", false},
		{"foo/bar", "foo/bar", true},
		{"foo/bar", "foo/bar/baz", false},
		{"foo/bar", "", true},
		{"", "", true},
		{"", "foo", false},
		{"foo/bar", "foo/b", false},
		{"out/Debug/obj/foo.o", "out/Debug", true},
		{"out/Debug/obj/foo.o", "out/Debu", false},
	}
	for _, tt := range tests {
		got := tt.path.HasPrefix(tt.prefix)
		if got != tt.want {
			t.Errorf("%q.HasPrefix(%q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestTrimPrefix(t *testing.T) {
	tests := []struct {
		path   Path
		prefix Path
		want   Path
	}{
		{"foo/bar/baz", "foo", "bar/baz"},
		{"foo/bar/baz", "foo/bar", "baz"},
		{"foo", "foo", ""},
		{"foobar", "foo", "foobar"},
		{"foo/bar", "", "foo/bar"},
		{"", "", ""},
		{"out/Debug/obj/foo.o", "out/Debug", "obj/foo.o"},
	}
	for _, tt := range tests {
		got := tt.path.TrimPrefix(tt.prefix)
		if got != tt.want {
			t.Errorf("%q.TrimPrefix(%q) = %q, want %q", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestRel(t *testing.T) {
	tests := []struct {
		path Path
		base Path
		want Path
	}{
		{"foo/bar/baz", "foo", "bar/baz"},
		{"foo/bar", "foo/bar", "."},
		{"foo/bar/baz", "foo/qux", "../bar/baz"},
	}
	for _, tt := range tests {
		got, err := tt.path.Rel(tt.base)
		if err != nil {
			t.Errorf("%q.Rel(%q) error: %v", tt.path, tt.base, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%q.Rel(%q) = %q, want %q", tt.path, tt.base, got, tt.want)
		}
	}
}

func TestIsAbs(t *testing.T) {
	tests := []struct {
		in   Path
		want bool
	}{
		{"/foo/bar", true},
		{"foo/bar", false},
		{"", false},
		{".", false},
		{"/", true},
		// Windows drive-absolute form is recognized on all hosts, so
		// paths originating from Windows depfiles/toolchains are not
		// misjoined onto a root.
		{"C:/foo", true},
		{"c:/foo", true},
		{"/usr/bin", true},
		{"out/Default/foo.o", false},
		// Ninja label-like strings with a mid-path ':' stay relative.
		{"foo:bar", false},
		// Bare drive letter and drive-relative paths are not absolute,
		// matching filepath drive-relative semantics.
		{"C:", false},
		{"C:foo", false},
	}
	for _, tt := range tests {
		got := tt.in.IsAbs()
		if got != tt.want {
			t.Errorf("%q.IsAbs() = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsEmpty(t *testing.T) {
	if !Path("").IsEmpty() {
		t.Error("empty path should be empty")
	}
	if Path("foo").IsEmpty() {
		t.Error("non-empty path should not be empty")
	}
}

func TestString(t *testing.T) {
	p := New("foo/bar")
	if p.String() != "foo/bar" {
		t.Errorf("String() = %q, want %q", p.String(), "foo/bar")
	}
}

func TestJoinRoot(t *testing.T) {
	tests := []struct {
		root string
		rel  Path
		want Path
	}{
		{"/home/user/chromium/src", "out/Debug/obj/foo.o", "/home/user/chromium/src/out/Debug/obj/foo.o"},
		{"/home/user/chromium/src/", "out/Debug/obj/foo.o", "/home/user/chromium/src/out/Debug/obj/foo.o"},
		{"", "foo/bar", "foo/bar"},
		{"/root", "", "/root"},
		// Absolute rel is returned as-is.
		{"/root", "/absolute/path", "/absolute/path"},
		// Dirty rel (e.g. double slash from a generator script with a
		// trailing-slash bug in its template) must be cleaned so hashfs
		// lookups match the normalized directory tree.
		{"/root", "services/metrics/public/cpp//ukm_decode.h", "/root/services/metrics/public/cpp/ukm_decode.h"},
		{"/root", "v8//third_party/foo.h", "/root/v8/third_party/foo.h"},
		{"/root", "./foo.h", "/root/foo.h"},
		{"/root/a/b", "../c", "/root/a/c"},
		// A UNC workspace root keeps its double slash through the join.
		{"//server/share/src", "out/foo.o", "//server/share/src/out/foo.o"},
		// A Windows drive-absolute rel (from a Windows depfile/toolchain)
		// is absolute on all hosts and must be returned unchanged rather
		// than joined onto root.
		{"/home/user/src", "C:/tools/clang.exe", "C:/tools/clang.exe"},
	}
	for _, tt := range tests {
		got := JoinRoot(tt.root, tt.rel)
		if got != tt.want {
			t.Errorf("JoinRoot(%q, %q) = %q, want %q", tt.root, tt.rel, got, tt.want)
		}
	}
}

func TestMapKey(t *testing.T) {
	// Verify Path works as a map key.
	m := map[Path]int{
		New("foo/bar"): 1,
		New("baz"):     2,
	}
	if m[Path("foo/bar")] != 1 {
		t.Error("map lookup failed for foo/bar")
	}
	if m[Path("baz")] != 2 {
		t.Error("map lookup failed for baz")
	}
}

func TestComparable(t *testing.T) {
	a := New("aaa/bbb")
	b := New("aaa/ccc")
	if a >= b {
		t.Errorf("expected %q < %q", a, b)
	}
}

func BenchmarkNew_Clean(b *testing.B) {
	// Already clean path: should be fast.
	s := "out/Debug/obj/base/allocator/allocator.o"
	b.ReportAllocs()
	for b.Loop() {
		_ = New(s)
	}
}

func BenchmarkNew_NeedsClean(b *testing.B) {
	s := "out/Debug/./obj/../obj/base/allocator/allocator.o"
	b.ReportAllocs()
	for b.Loop() {
		_ = New(s)
	}
}

func BenchmarkJoinPath(b *testing.B) {
	base := Path("out/Debug/obj")
	elem := Path("base/allocator/allocator.o")
	b.ReportAllocs()
	for b.Loop() {
		_ = base.JoinPath(elem)
	}
}

// BenchmarkJoinRootHit measures the warm cache-hit path: the same
// (root, rel) pair every iteration, so all but the first lookup hit the
// cache. This is the dominant case in a build (~1M hits per unique pair).
func BenchmarkJoinRootHit(b *testing.B) {
	root := "/home/user/chromium/src"
	rel := Path("out/Debug/obj/base/allocator/allocator.o")
	b.ReportAllocs()
	for b.Loop() {
		_ = JoinRoot(root, rel)
	}
}

// BenchmarkJoinRootMiss measures the cold cache-miss path: the join, the
// clean, and the store. A bounded set of distinct rels is cycled, and the
// cache is reset (untimed) each time the set is exhausted, so every
// iteration stays on the miss path while retained memory stays bounded by
// setSize rather than by b.N.
func BenchmarkJoinRootMiss(b *testing.B) {
	const root = "/home/user/chromium/src"
	const setSize = 4096
	rels := make([]Path, setSize)
	for i := range rels {
		rels[i] = Path(fmt.Sprintf("out/Default/obj/pkg%d/file%d.o", i/32, i))
	}
	b.ReportAllocs()
	i := setSize // force a reset (and thus a full set of misses) on entry
	for b.Loop() {
		if i == setSize {
			i = 0
			b.StopTimer()
			for s := range joinShards {
				joinShards[s].mu.Lock()
				clear(joinShards[s].m)
				joinShards[s].mu.Unlock()
			}
			b.StartTimer()
		}
		_ = JoinRoot(root, rels[i])
		i++
	}
}

func TestJoinRootCacheConcurrent(t *testing.T) {
	const root = "/home/user/chromium/src"
	rels := make([]Path, 2048)
	for i := range rels {
		rels[i] = Path(fmt.Sprintf("out/Default/obj/pkg%d/file%d.o", i/32, i))
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 4 {
				for _, rel := range rels {
					p := JoinRoot(root, rel)
					want := Path(root + "/" + string(rel))
					if p != want {
						t.Errorf("JoinRoot(%q, %q) = %q, want %q", root, rel, p, want)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

func BenchmarkHasPrefix(b *testing.B) {
	p := Path("out/Debug/obj/base/allocator/allocator.o")
	prefix := Path("out/Debug")
	b.ReportAllocs()
	for b.Loop() {
		_ = p.HasPrefix(prefix)
	}
}
