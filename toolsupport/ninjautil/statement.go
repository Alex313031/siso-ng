// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"fmt"
)

// statement is a logical statement in ninja file.
type statement struct {
	pos int // statement position in a file
	t   statementType
	s   int // start position
	v   int // position after keyword, or '='
	e   int // end position
}

type statementType int

const (
	statementNone statementType = iota
	statementVarDecl
	statementPool
	statementPoolVar
	statementRule
	statementRuleVar
	statementBuild
	statementBuildVar
	statementDefault
	statementInclude
	statementSubninja
)

func (s statementType) String() string {
	switch s {
	case statementVarDecl:
		return "var-decl"
	case statementPool:
		return "pool"
	case statementPoolVar:
		return "pool-var"
	case statementRule:
		return "rule"
	case statementRuleVar:
		return "rule-var"
	case statementBuild:
		return "build"
	case statementBuildVar:
		return "build-var"
	case statementDefault:
		return "default"
	case statementInclude:
		return "include"
	case statementSubninja:
		return "subninja"
	}
	return fmt.Sprintf("unknown[%d]", int(s))
}

// isStatement checks buf[i:] is a statement for name, and
// returns position after name if so.
func isStatement(buf []byte, i int, name []byte) (int, bool) {
	if !bytes.HasPrefix(buf[i:], name) {
		return -1, false
	}
	ch := buf[i+len(name)]
	switch ch {
	case ' ', '\t':
		return i + len(name) + 1, true
	}
	return -1, false
}
