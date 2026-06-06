// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

// arena is pre-allocated limited contiguous memory to reduce allocations and impact to GC.
type arena[T any] struct {
	allocs []T
	next   int
}

// reserve allocates n of T.
func (a *arena[T]) reserve(n int) {
	a.allocs = make([]T, n)
}

// new returns a *T from arena, allocated by reserve.
func (a *arena[T]) new() *T {
	p := &a.allocs[a.next]
	a.next++
	return p
}

// chunk returns new sub arena of size n.
func (a *arena[T]) chunk(n int) arena[T] {
	s := a.allocs[a.next : a.next+n : a.next+n]
	a.next += n
	return arena[T]{
		allocs: s,
	}
}

// slice returns slices of size n.
func (a *arena[T]) slice(n int) []T {
	s := a.allocs[a.next : a.next+n : a.next+n]
	a.next += n
	return s
}

// len returns number of used allocations in this arena.
func (a *arena[T]) len() int {
	return a.next
}

// used returns used allocations in this arena.
func (a *arena[T]) used() []T {
	return a.allocs[:a.next]
}

// at returns *T at i.
func (a *arena[T]) at(i int) *T {
	return &a.allocs[i]
}
