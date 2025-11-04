// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"fmt"
	"sync"
)

// evalString is string to be evaluated (binding value, path).
type evalString struct {
	pos int // statement position
	// original string
	v []byte

	// number of escapes used in v.
	esc int
}

// evalEnv is environment to evaluate evalString.
type evalEnv interface {
	// lookupVar looks up var by name before pos (when pos >= 0).
	// when pos < 0, no pos check.
	lookupVar(pos int, name []byte) (evalString, bool)
}

// evalSetEnv is environment to set evalString for name.
type evalSetEnv interface {
	evalEnv
	setVar([]byte, evalString)
}

// parseEvalString parses v as evalString.
func parseEvalString(v []byte) (evalString, error) {
	i := skipSpaces(v, 0, whitespaceChar)
	v = v[i:]
	svc := simpleVarnameChar
	esc := 0
loop:
	for i := 0; i < len(v); i++ {
		j := bytes.IndexByte(v[i:], '$')
		if j < 0 {
			break
		}
		i += j
		if i+1 >= len(v) {
			return evalString{}, fmt.Errorf("invalid $-escape")
		}
		ch := v[i+1]
		esc++
		switch ch {
		case ' ', '\t', ':', '$', '\n':
			i++
			continue loop
		case '\r':
			if i+2 >= len(v) || v[i+2] != '\n' {
				return evalString{}, fmt.Errorf("invalid $-escape for \\r")
			}
			i += 2
			continue loop
		case '{':
			j := bytes.IndexByte(v[i+2:], '}')
			if j < 0 {
				return evalString{}, fmt.Errorf("unclosed ${")
			}
			i = i + 2 + j
			continue loop
		}
		// simplevar?
		if !svc.contains(ch) {
			return evalString{}, fmt.Errorf("invalid $-escape %q in %q", ch, v)
		}
		for j := i + 1; j < len(v); j++ {
			if !svc.contains(v[j]) {
				i = j - 1
				break
			}
		}
	}
	v = bytes.TrimSuffix(v, []byte("\r\n"))
	v = bytes.TrimSuffix(v, []byte("\n"))
	return evalString{v: v, esc: esc}, nil
}

type evalLookupStack struct {
	stack [][]byte
}

var evalLookupStackPool = sync.Pool{
	New: func() any {
		return &evalLookupStack{
			stack: make([][]byte, 0, 256),
		}
	},
}

// evaluate evaluates val in env.
// returned []byte will be valid until buf is reset or next evaluate.
func evaluate(env evalEnv, buf *bytes.Buffer, val evalString) ([]byte, error) {
	if len(val.v) == 0 {
		return nil, nil
	}
	if val.esc == 0 {
		return val.v, nil
	}
	buf.Reset()
	s := evalLookupStackPool.Get().(*evalLookupStack)
	defer func() {
		s.stack = s.stack[:0]
		evalLookupStackPool.Put(s)
	}()
	return evaluateAppend(env, buf, val, s.stack)
}

// evaluateAppend is helper function of evaluate.
func evaluateAppend(env evalEnv, buf *bytes.Buffer, val evalString, lookups [][]byte) ([]byte, error) {
	if len(val.v) == 0 {
		return nil, nil
	}
	if val.esc == 0 {
		buf.Write(val.v)
		return val.v, nil
	}
	for i := 0; i < len(val.v); i++ {
		if val.v[i] == '$' {
			i++
			if i == len(val.v) {
				return nil, fmt.Errorf("invalid $-escape? %q", val.v)
			}
			switch val.v[i] {
			case ' ', ':', '$': // escape
				buf.WriteByte(val.v[i])
				continue
			case '\r', '\n':
				// whitespace at the beginning of a line after a line continuation is also stripped.
				for j := range len(val.v[i+1:]) {
					if !whitespaceChar.contains(val.v[i+j]) {
						i += j - 1
						break
					}
				}
				continue
			}
			var varname []byte
			if val.v[i] == '{' {
				j := bytes.IndexByte(val.v[i+1:], '}')
				if j < 0 {
					return nil, fmt.Errorf("unclosed ${ %q", val.v)
				}
				varname = val.v[i+1 : i+1+j]
				i = i + 1 + j
			} else if simpleVarnameChar.contains(val.v[i]) {
				for j := range len(val.v[i:]) {
					if !simpleVarnameChar.contains(val.v[i+j]) {
						varname = val.v[i : i+j]
						i = i + j - 1
						break
					}
				}
				if len(varname) == 0 {
					varname = val.v[i:]
					i = len(val.v)
				}
			} else {
				return nil, fmt.Errorf("invalid $-escape? %q", val.v)
			}
			if env == nil {
				continue
			}
			if len(lookups) > 255 {
				return nil, fmt.Errorf("cycle in rule variable %q (%q)", varname, lookups)
			}
			v, ok := env.lookupVar(val.pos, varname)
			if ok {
				_, err := evaluateAppend(env, buf, v, append(lookups, varname))
				if err != nil {
					return nil, err
				}
			}
			continue
		}
		buf.WriteByte(val.v[i])
	}
	return buf.Bytes(), nil
}
