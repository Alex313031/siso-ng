// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"hash/maphash"
	"sync"
)

// binding is a binding for 'name = value'.
type binding struct {
	name  []byte
	value evalString
}

// bindingList is a list of bindings.
type bindingList struct {
	mu   sync.Mutex
	list []binding
}

// shardBindings are sharded binding map for fast concurrent updates.
type shardBindings struct {
	seed   maphash.Seed
	shards []bindingList
}

// newShardBindings creates new shardBindings for estimated size n.
func newShardBindings(n int) *shardBindings {
	sb := &shardBindings{}
	sb.init(maphash.MakeSeed(), n)
	return sb
}

// init initializes shardBinding with seed and estimated size n.
func (sb *shardBindings) init(seed maphash.Seed, n int) {
	sb.seed = seed
	// prepare sufficient buckets for size n.
	for size := 1; ; size <<= 1 {
		if n/size < 8 {
			sb.shards = make([]bindingList, size)
			return
		}
	}
}

// get looks up evalString associated with key before pos, if any.
// when pos < 0, no pos check.
func (sb *shardBindings) get(pos int, key []byte) (evalString, bool) {
	var zero evalString
	if sb == nil {
		return zero, false
	}
	if len(sb.shards) == 0 {
		return zero, false
	}
	var m *bindingList
	if len(sb.shards) == 1 {
		m = &sb.shards[0]
	} else {
		h := maphash.Bytes(sb.seed, key)
		m = &sb.shards[h&uint64(len(sb.shards)-1)]
	}
	var ret evalString
	var found bool
	// m.list is not ordered by pos, but need to find the largest
	// value.pos less than given pos (i.e. shadowed bindings).
	for i := len(m.list) - 1; i >= 0; i-- {
		if pos >= 0 && m.list[i].value.pos >= pos {
			continue
		}
		if found && m.list[i].value.pos < ret.pos {
			continue
		}
		if bytes.Equal(key, m.list[i].name) {
			ret = m.list[i].value
			found = true
		}
	}
	return ret, found
}

// set sets "key=value".
func (sb *shardBindings) set(key []byte, value evalString) {
	if sb == nil {
		return
	}
	if len(sb.shards) == 0 {
		return
	}
	var m *bindingList
	if len(sb.shards) == 1 {
		m = &sb.shards[0]
	} else {
		h := maphash.Bytes(sb.seed, key)
		m = &sb.shards[h&uint64(len(sb.shards)-1)]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.list = append(m.list, binding{
		name:  key,
		value: value,
	})
}

// keys returns a list of keys in shardBindings.
func (sb *shardBindings) keys() []string {
	if sb == nil {
		return nil
	}
	var keys []string
	for i := range sb.shards {
		m := &sb.shards[i]
		m.mu.Lock()
		for _, b := range m.list {
			keys = append(keys, string(b.name))
		}
		m.mu.Unlock()
	}
	return keys
}
