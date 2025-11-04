// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

// slab is pre-batch allocated (without limits) memory to reduce allocations and impact to GC.
type slab[T any] struct {
	allocs []T
	next   int
}

// newSlab creates new slab for n of T.
func newSlab[T any](n int) slab[T] {
	return slab[T]{
		allocs: make([]T, n),
	}
}

// new returns *T from slab.
func (s *slab[T]) new() *T {
	if s.next >= len(s.allocs) {
		s.allocs = make([]T, len(s.allocs))
		s.next = 0
	}
	p := &s.allocs[s.next]
	s.next++
	return p
}

// unnew puts last *T returned by new back in slab.
func (s *slab[T]) unnew() {
	if s.next <= 0 {
		return
	}
	s.next--
}

// slice returns slices [n]T.
func (s *slab[T]) slice(n int) []T {
	if s.next >= len(s.allocs) {
		s.allocs = make([]T, len(s.allocs))
		s.next = 0
	}
	// for large n, it will create new slice to avoid fragmentation.
	if n >= len(s.allocs)/2 {
		return make([]T, n)
	}
	if s.next+n > len(s.allocs) {
		return make([]T, n)
	}
	r := s.allocs[s.next : s.next+n : s.next+n]
	s.next += n
	return r
}
