// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import "bytes"

// findNextLine returns offset of next logical line from buf[s:].
// i.e. for `n := findNextLine(buf, s)`, buf[n:] will be next logical line,
// or end of buf.
func findNextLine(buf []byte, s int) int {
	for {
		i := bytes.IndexByte(buf[s:], '\n')
		if i < 0 { // each EOF?
			return len(buf)
		}
		if i > 1 {
			if buf[s+i-2] == '$' && buf[s+i-1] == '\r' {
				s = s + i + 1
				continue
			}
		}
		if i > 0 {
			if buf[s+i-1] == '$' {
				s = s + i + 1
				continue
			}
		}
		return s + i + 1
	}
}

type charmap [8]uint32

func (m *charmap) set(ch byte) {
	(*m)[ch>>5] |= 1 << uint(ch&31)
}

func (m *charmap) contains(ch byte) bool {
	return (*m)[ch>>5]&(1<<uint(ch&31)) != 0
}

// [a-zA-Z0-9_.-]
var varnameChar charmap

// [a-zA-Z0-9_-]
var simpleVarnameChar charmap

func init() {
	for ch := byte('a'); ch <= 'z'; ch++ {
		varnameChar.set(ch)
		simpleVarnameChar.set(ch)
	}
	for ch := byte('A'); ch <= 'Z'; ch++ {
		varnameChar.set(ch)
		simpleVarnameChar.set(ch)
	}
	for ch := byte('0'); ch <= '9'; ch++ {
		varnameChar.set(ch)
		simpleVarnameChar.set(ch)
	}
	varnameChar.set('_')
	varnameChar.set('.')
	varnameChar.set('-')
	simpleVarnameChar.set('_')
	simpleVarnameChar.set('-')
}

var nonLiteralChar charmap
var whitespaceChar charmap

func init() {
	for _, ch := range []byte("$ :\t\r\n|\000") {
		nonLiteralChar.set(ch)
	}
	// https://github.com/ninja-build/ninja/issues/952
	// tab is not considered as whitespace in Ninja.
	for _, ch := range []byte(" \r\n") {
		whitespaceChar.set(ch)
	}
}

// indexBytesAny returns offset in buf where the byte is in charmap.
func indexBytesAny(buf []byte, cm charmap) int {
	for i := range buf {
		if cm.contains(buf[i]) {
			return i
		}
	}
	return len(buf)
}

// skipBytesAny returns offset in buf where the byte is not in charmap.
func skipBytesAny(buf []byte, cm charmap) int {
	for i := range buf {
		if !cm.contains(buf[i]) {
			return i
		}
	}
	return len(buf)
}

// skipSpaces returns next offset of non-whitespace char in buf[pos:].
// it will skip "$\n" or "$\r\n".
func skipSpaces(buf []byte, pos int, whitespaceChar charmap) int {
	n := 0
	for {
		i := skipBytesAny(buf[pos+n:], whitespaceChar)
		n += i
		if len(buf) == pos+n {
			return n
		}
		if buf[pos+n] != '$' {
			return n
		}
		if pos+n+1 < len(buf) {
			switch buf[pos+n+1] {
			case '\n', ' ', '\t':
				n += 2
				continue
			case '\r':
				if pos+n+2 < len(buf) && buf[pos+n+2] == '\n' {
					n += 3
					continue
				}
			}
		}
		return n
	}
}
