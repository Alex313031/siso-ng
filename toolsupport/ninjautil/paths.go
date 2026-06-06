// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"fmt"
)

// pathParser is a parser for path lists.
type pathParser struct {
	buf []byte
	pos int

	// put charmap locally
	nonLiteralChar charmap
	whitespaceChar charmap
}

// newPathParser returns new path parser on buf.
func newPathParser(buf []byte) *pathParser {
	return &pathParser{
		buf:            buf,
		nonLiteralChar: nonLiteralChar,
		whitespaceChar: whitespaceChar,
	}
}

// ident returns a next identifier.
func (p *pathParser) ident() ([]byte, error) {
	p.pos += skipSpaces(p.buf, p.pos, p.whitespaceChar)
	if len(p.buf[p.pos:]) == 0 {
		return nil, fmt.Errorf("no ident")
	}
	buf := p.buf[p.pos:]
	e := skipBytesAny(buf, varnameChar)
	p.pos += e
	return buf[:e], nil
}

// pathList parses path lists, appends paths in pl, and returns number of parsed paths.
func (p *pathParser) pathList(pl []evalString) ([]evalString, int) {
	n := 0
	for {
		path := p.path()
		if len(path.v) == 0 {
			break
		}
		pl = append(pl, path)
		n++
	}
	return pl, n
}

// path parses a path.
func (p *pathParser) path() evalString {
	var path evalString
	p.pos += skipSpaces(p.buf, p.pos, p.whitespaceChar)
	if len(p.buf[p.pos:]) == 0 {
		return path
	}
	buf := p.buf[p.pos:]
	i := 0
	for {
		n := indexBytesAny(buf[i:], p.nonLiteralChar)
		if i+n == len(buf) {
			p.pos += len(buf)
			path.v = buf
			return path
		}
		if buf[i+n] == '$' {
			if bytes.HasPrefix(buf[i+n+1:], []byte("\r\n")) ||
				bytes.HasPrefix(buf[i+n+1:], []byte("\n")) {
				// $\n or $\r\n will be treated as whitespace.
				p.pos += i + n
				path.v = buf[:i+n]
				return path
			}
			// escaped, or varref.
			i += n + 2
			path.pos = -1
			path.esc++
			continue
		}
		p.pos += i + n
		path.v = buf[:i+n]
		return path
	}
}

// colon checks if next is ":" and moves the position forward.
func (p *pathParser) colon() bool {
	p.pos += skipSpaces(p.buf, p.pos, p.whitespaceChar)
	if p.pos == len(p.buf) {
		return false
	}
	if p.buf[p.pos] != ':' {
		return false
	}
	p.pos++
	return true
}

// pipe checks if next is "|" and moves the position forward.
func (p *pathParser) pipe() bool {
	p.pos += skipSpaces(p.buf, p.pos, p.whitespaceChar)
	if p.pos == len(p.buf) {
		return false
	}
	if p.buf[p.pos] != '|' {
		return false
	}
	if p.pos+1 == len(p.buf) {
		p.pos++
		return true
	}
	switch p.buf[p.pos+1] {
	case '|': // ||
		return false
	case '@': // |@
		return false
	}
	p.pos++
	return true
}

// pipe2 checks if next is "||" and moves the position forward.
func (p *pathParser) pipe2() bool {
	p.pos += skipSpaces(p.buf, p.pos, p.whitespaceChar)
	if p.pos+1 >= len(p.buf) {
		return false
	}
	if bytes.HasPrefix(p.buf[p.pos:], []byte("||")) {
		p.pos += 2
		return true
	}
	return false
}

// pipeAt checks if next is "|@" and moves the position forward.
func (p *pathParser) pipeAt() bool {
	p.pos += skipSpaces(p.buf, p.pos, p.whitespaceChar)
	if p.pos+1 >= len(p.buf) {
		return false
	}
	if bytes.HasPrefix(p.buf[p.pos:], []byte("|@")) {
		p.pos += 2
		return true
	}
	return false
}
