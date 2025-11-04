// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package ninjautil

import (
	"strings"
)

func isShellSafeChar(ch rune) bool {
	if 'A' <= ch && ch <= 'Z' {
		return true
	}
	if 'a' <= ch && ch <= 'z' {
		return true
	}
	if '0' <= ch && ch <= '9' {
		return true
	}
	switch ch {
	case '_', '+', '-', '.', '/':
		return true
	}
	return false
}

func needShellEscape(s string) bool {
	for _, ch := range s {
		if !isShellSafeChar(ch) {
			return true
		}
	}
	return false
}

func shellEscape(s string) string {
	if !needShellEscape(s) {
		return s
	}
	var sb strings.Builder
	sb.WriteString("'")
	for _, ch := range s {
		if ch == '\'' {
			sb.WriteString(`'\''`)
			continue
		}
		sb.WriteRune(ch)
	}
	sb.WriteString("'")
	return sb.String()
}
