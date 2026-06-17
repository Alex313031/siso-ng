// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package ninjautil

import "strings"

func isShellSafeChar(ch rune) bool {
	switch ch {
	case ' ', '"':
		return false
	default:
		return true
	}
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
	sb.WriteRune('"')
	consecutiveBackslashCount := 0
	spanBegin := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '\\':
			consecutiveBackslashCount++
		case '"':
			sb.WriteString(s[spanBegin:i])
			sb.WriteString(strings.Repeat(`\`, consecutiveBackslashCount+1))
			spanBegin = i
			consecutiveBackslashCount = 0
		default:
			consecutiveBackslashCount = 0
		}
	}
	sb.WriteString(s[spanBegin:])
	sb.WriteString(strings.Repeat(`\`, consecutiveBackslashCount))
	sb.WriteRune('"')
	return sb.String()
}
