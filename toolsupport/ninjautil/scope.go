// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"fmt"
	"io"
	"sort"
)

// fileScope is ninja rule/binding scope (in a file).
type fileScope struct {
	rules    *ruleMap
	bindings *shardBindings
	parent   *fileScope
}

// newFileScope creates new fileScope.
func newFileScope(parent *fileScope) *fileScope {
	return &fileScope{
		parent: parent,
	}
}

// lookupRule looks up rule by name in the scope and its parents, if any.
func (s *fileScope) lookupRule(ruleName []byte) (*rule, bool) {
	r, ok := s.rules.lookupRule(ruleName)
	if ok {
		return r, true
	}
	if s.parent != nil {
		return s.parent.lookupRule(ruleName)
	}
	if string(ruleName) == "phony" {
		return phonyRule, true
	}
	return nil, false
}

// setRule sets rule in the scope.
func (s *fileScope) setRule(rule *rule) error {
	return s.rules.setRule(rule)
}

// lookupVar looks up variable binding in the scope and its parents, if any.
func (s *fileScope) lookupVar(pos int, name []byte) (evalString, bool) {
	v, ok := s.bindings.get(pos, name)
	if ok {
		return v, ok
	}
	if s.parent != nil {
		return s.parent.lookupVar(-1, name)
	}
	return evalString{}, false
}

// setVar sets variable "name=value".
func (s *fileScope) setVar(name []byte, value evalString) {
	s.bindings.set(name, value)
}

// Print prints scope in w.
func (s *fileScope) Print(w io.Writer) {
	if s == nil {
		return
	}
	if s.parent != nil {
		s.parent.Print(w)
	}
	keys := s.bindings.keys()
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	for _, key := range keys {
		ev, _ := s.bindings.get(-1, []byte(key))
		fmt.Fprintf(w, "%s = %s\n", key, ev.v)
	}
	fmt.Fprintln(w)
}

// buildScope is a binding scope for a build.
type buildScope struct {
	buf        []byte
	statements []statement
}

// lookupVar looks up var in a build statement, if any.
func (b *buildScope) lookupVar(pos int, name []byte) (evalString, bool) {
	for i := len(b.statements) - 1; i >= 0; i-- {
		st := b.statements[i]
		if pos >= 0 && st.pos >= pos {
			continue
		}
		key := bytes.TrimSpace(b.buf[st.s:st.v])
		if !bytes.Equal(key, name) {
			continue
		}
		v := b.buf[st.v+1 : st.e]
		v = bytes.TrimSpace(v)
		return evalString{
			v:   v,
			esc: bytes.Count(v, []byte{'$'}),
		}, true
	}
	return evalString{}, false
}

// set sets build bindings.
func (b *buildScope) set(buf []byte, statements []statement) {
	b.buf = buf
	b.statements = statements
}

// Print prints bindings in w.
func (b *buildScope) Print(w io.Writer) {
	if len(b.statements) == 0 {
		return
	}
	for _, st := range b.statements {
		fmt.Fprintf(w, "%s", b.buf[st.s:st.e])
	}
}
