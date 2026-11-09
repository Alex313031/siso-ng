// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package makeutil provides utilities for make.
package makeutil

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"strings"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
)

// IgnoreMissingOut controls whether to report missing out in depfile as
// error (false) or not (true).
var IgnoreMissingOut bool

// ParseDepsFile parses *.d file in fname on fsys.
func ParseDepsFile(ctx context.Context, fsys fs.FS, fname string) ([]string, error) {
	if fname == "" {
		return nil, nil
	}
	b, err := fs.ReadFile(fsys, fname)
	if err != nil {
		return nil, err
	}
	deps, err := ParseDeps(ctx, b)
	if log.V(1) {
		clog.Infof(ctx, "deps %s => %s: %v", fname, deps, err)
	}
	return deps, err
}

// ParseDeps parses deps and returns a list of inputs.
func ParseDeps(ctx context.Context, b []byte) ([]string, error) {
	// deps contents
	// <output>: <input> ...
	// <input> is space separated
	// '\'+newline is space
	// '\'+space is escaped space (not separator)
	s := b
	var token string
	seen := make(map[string]bool)
	var inputs []string
depLines:
	for len(s) > 0 {
		// skip outputs until ':'
		i := bytes.IndexByte(s, ':')
		if i < 0 {
			break
		}
		out := bytes.TrimSpace(s[:i])
		if len(out) == 0 {
			if IgnoreMissingOut {
				clog.Warningf(ctx, "missing output in deps. depfile should be `<target>: <dependencyList>`")
			} else {
				return nil, fmt.Errorf("missing output in deps. depfile should be `<target>: <dependencyList>`")
			}
		}
		// collect inputs
		for s = s[i+1:]; len(s) > 0; {
			token, s = nextToken(s)
			switch token {
			case "":
				continue
			case ":":
				return nil, fmt.Errorf("multiple colon in dep line. depfile should be `<target>: <dependencyList>`")
			case "\n":
				continue depLines
			}
			if seen[token] {
				continue
			}
			seen[token] = true
			inputs = append(inputs, token)
		}
	}
	return inputs, nil
}

func nextToken(s []byte) (string, []byte) {
	// skip spaces
	var i int
skipSpaces:
	for i = 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '\n' {
			i++
			continue
		}
		if s[i] == '\\' && i+2 < len(s) && s[i+1] == '\r' && s[i+2] == '\n' {
			i += 2
			continue
		}
		switch s[i] {
		case '\n':
			return "\n", s[i+1:]
		case ' ', '\t', '\r':
			continue
		default:
			break skipSpaces
		}
	}
	s = s[i:]
	// Fast path: most depfile tokens are unescaped paths. Scan to the first
	// delimiter or escape and return the input sub-slice in one allocation.
	for j := range s {
		switch s[j] {
		case '\\':
			// escape sequence; fall through to Builder slow path.
			return nextTokenSlow(s, j)
		case ' ', '\t', '\n', '\r':
			return string(s[:j]), s[j:]
		case ':':
			switch j {
			case 0:
				return ":", s[1:]
			case 1:
				// <drive>: ? keep scanning.
			default:
				return string(s[:j]), s[j:]
			}
		}
	}
	return string(s), nil
}

// nextTokenSlow handles tokens with backslash escapes. nextToken calls
// it on the first escape; the prefix s[:start] has no escapes or
// delimiters, so it seeds the Builder.
func nextTokenSlow(s []byte, start int) (string, []byte) {
	// String() returns the builder buffer uncopied, so keep it token-sized: a
	// depfile-sized builder would let one short escaped token pin a huge allocation.
	var sb strings.Builder
	sb.Write(s[:start])
	for i := start; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case ' ':
				sb.WriteByte(s[i])
			case '\r', '\n':
				// '\'+newline is space
				return sb.String(), s[i-1:]
			default:
				sb.WriteByte('\\')
				sb.WriteByte(s[i])
			}
			continue
		}
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			return sb.String(), s[i:]
		case ':':
			switch sb.Len() {
			case 0:
				return ":", s[i+1:]
			case 1:
				// <drive>: ?
			default:
				return sb.String(), s[i:]
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String(), nil
}
