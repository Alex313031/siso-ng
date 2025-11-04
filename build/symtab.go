// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"strings"
	"sync"
)

type symtab struct {
	// Map of string => string, where both key and value are the same.
	m sync.Map
}

func (s *symtab) Lookup(v string) (string, bool) {
	vv, ok := s.m.Load(v)
	if !ok {
		return "", false
	}
	return vv.(string), true
}

func (s *symtab) Intern(v string) string {
	v = strings.Clone(v)
	vv, _ := s.m.LoadOrStore(v, v)
	return vv.(string)
}

func (s *symtab) InternSlice(list []string) []string {
	ret := make([]string, 0, len(list))
	for _, v := range list {
		vv, ok := s.m.Load(v)
		if ok {
			ret = append(ret, vv.(string))
			continue
		}
		// Make a copy of the string.
		// In case string is substring of large string, if it is used as
		// intern value, the large string would be kept in memory.
		// https://github.com/golang/go/issues/40200
		v = strings.Clone(v)
		vv, _ = s.m.LoadOrStore(v, v)
		ret = append(ret, vv.(string))
	}
	return ret
}
