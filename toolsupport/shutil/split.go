// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package shutil

import (
	"bytes"
	"fmt"
	"strings"
)

// Split splits a command line.
// It would return error for complicated pipe line.
func Split(cmdline string) ([]string, error) {
	var args []string
	buf := bytes.NewBuffer(make([]byte, 0, len(cmdline)))
	sb := buf
	escaped := false
	var qch rune // 0, '"', '\''
	inspace := false
	si := 0
	cmdline = strings.TrimSpace(cmdline)
	for i, ch := range cmdline {
		if escaped {
			sb.WriteRune(ch)
			escaped = false
			si = i + 1
			continue
		}
		if qch != 0 {
			if ch == '\\' {
				escaped = true
				continue
			}
			if qch == ch {
				qch = 0
			} else {
				sb.WriteRune(ch)
			}
			si = i + 1
			continue
		}
		switch ch {
		case '\\':
			if sb == nil {
				buf.Reset()
				sb = buf
			}
			if si < i {
				sb.WriteString(cmdline[si:i])
			}
			inspace = false
			escaped = true
			si = i + 1
			continue
		case '"', '\'':
			if sb == nil {
				buf.Reset()
				sb = buf
			}
			if si < i {
				sb.WriteString(cmdline[si:i])
			}
			inspace = false
			qch = ch
			si = i + 1
			continue
		case ' ', '\t':
			if inspace {
				si = i + 1
				continue
			}
			inspace = true
			var arg string
			if sb != nil {
				arg = sb.String()
				sb = nil
			} else if si < i {
				arg = cmdline[si:i]
			}
			si = i + 1
			args = append(args, arg)
			continue
		case ';', '&', '|', '<', '>', '$', '#', '`':
			return nil, fmt.Errorf("failed to split: cmdline contains shell metachar %c", ch)
		default:
			if !inspace && sb != nil {
				sb.WriteRune(ch)
				si = i + 1
			}
			inspace = false
		}
	}
	if escaped {
		return nil, fmt.Errorf(`failed to split: end with \-escape`)
	}
	if qch != 0 {
		return nil, fmt.Errorf("failed to split: unclosed quote")
	}
	if sb != nil {
		args = append(args, sb.String())
	} else if si < len(cmdline) {
		args = append(args, cmdline[si:])
	}
	if len(args) >= 1 && strings.Contains(args[0], "=") {
		// if initial args contains =, it would set env var and need to invoke via sh
		// TODO(ukai): parse env overrides?
		return nil, fmt.Errorf("argv[0] is env set %q", args[0])
	}
	return args, nil
}
