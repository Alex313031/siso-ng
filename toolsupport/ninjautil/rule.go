// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"fmt"
	"hash/maphash"
	"sync/atomic"
)

// rule represents a build rule.
// Further reading: https://ninja-build.org/manual.html#_rules
// TODO(b/267409605): Add tests for Rule methods.
type rule struct {
	next     *rule // for ruleMap
	name     string
	bindings []binding
}

// Name returns rule's name.
func (r *rule) Name() string { return r.name }

// setVar adds bindings to the rule.
func (r *rule) setVar(key []byte, val evalString) {
	r.bindings = append(r.bindings, binding{
		name:  key,
		value: val,
	})
}

// lookupVar looks up a binding in the rule.
func (r *rule) lookupVar(pos int, key []byte) (evalString, bool) {
	var ret evalString
	var found bool
	for i := len(r.bindings) - 1; i >= 0; i-- {
		b := r.bindings[i]
		if pos >= 0 && b.value.pos >= pos {
			continue
		}
		if found && b.value.pos < ret.pos {
			continue
		}
		if bytes.Equal(b.name, key) {
			ret = b.value
			found = true
		}
	}
	return ret, found
}

// ruleMap is a map rules by its name for fast concurrent updates.
type ruleMap struct {
	seed  maphash.Seed
	rules []atomic.Pointer[rule]
}

// newRuleMap creates new ruleMap for size n.
func newRuleMap(n int) *ruleMap {
	rm := &ruleMap{
		seed: maphash.MakeSeed(),
	}
	// numRulesPerBuckets is the number of rules per buckets.
	// We want to keep it small to avoid contention.
	const numRulesPerBuckets = 8
	// prepare sufficient buckets for size n.
	numBuckets := 1
	for n/numBuckets >= numRulesPerBuckets {
		numBuckets <<= 1
	}
	rm.rules = make([]atomic.Pointer[rule], numBuckets)
	return rm
}

// setRule sets a rule in ruleMap.
func (rm *ruleMap) setRule(r *rule) error {
	if rm == nil || len(rm.rules) == 0 {
		return fmt.Errorf("no rule map")
	}
	var slot *atomic.Pointer[rule]
	if len(rm.rules) == 1 {
		slot = &rm.rules[0]
	} else {
		h := maphash.String(rm.seed, r.name)
		slot = &rm.rules[h&uint64(len(rm.rules)-1)]
	}
	var search *rule
	for {
		head := slot.Load()
		search = head
		for search != nil {
			if search.name == r.name {
				return fmt.Errorf("duplicate rule %q", r.name)
			}
			search = search.next
		}
		r.next = head
		if slot.CompareAndSwap(head, r) {
			return nil
		}
	}
}

// lookupRule looks up rule by its name.
func (rm *ruleMap) lookupRule(name []byte) (*rule, bool) {
	if rm == nil || len(rm.rules) == 0 {
		return nil, false
	}
	var r *rule
	if len(rm.rules) == 1 {
		r = rm.rules[0].Load()
	} else {
		h := maphash.Bytes(rm.seed, name)
		r = rm.rules[h&uint64(len(rm.rules)-1)].Load()
	}
	for r != nil {
		if bytes.Equal([]byte(r.name), name) {
			return r, true
		}
		r = r.next
	}
	return nil, false
}
