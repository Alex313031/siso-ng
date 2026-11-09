// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package blob

import (
	"go.chromium.org/build/hashigo/digest"
)

// Store works as an in-memory content addressable storage.
type Store struct {
	m map[digest.Digest]Data
}

// NewStore creates Store.
func NewStore() *Store {
	return &Store{
		m: make(map[digest.Digest]Data),
	}
}

// Set sets data to the store.
func (s *Store) Set(d Data) {
	s.m[d.Digest()] = d
}

// Get gets data from store by the digest.
func (s *Store) Get(dg digest.Digest) (Data, bool) {
	v, ok := s.m[dg]
	return v, ok
}

// GetSource gets source from the store.
func (s *Store) GetSource(dg digest.Digest) (Source, bool) {
	v, ok := s.Get(dg)
	if !ok {
		return nil, false
	}
	return v.source, true
}

// Delete deletes digest from the store.
func (s *Store) Delete(dg digest.Digest) {
	delete(s.m, dg)
}

// Size returns the number of digests in the store.
func (s *Store) Size() int {
	if s == nil {
		return 0
	}
	return len(s.m)
}

// List returns a list of the digests of the stored data.
func (s *Store) List() []digest.Digest {
	digests := make([]digest.Digest, 0, len(s.m))
	for k := range s.m {
		digests = append(digests, k)
	}
	return digests
}
