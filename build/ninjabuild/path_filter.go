// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"path"
	"strings"

	"go.chromium.org/build/siso/o11y/clog"
)

// PathFilter specifies filter rules for action inputs.
//
// If Patterns is set, it ignores Excludes and Includes.
//
// If no Patterns is set, checks Excludes, then Includes
// for backward compatibility.
type PathFilter struct {
	// glob pattern to match.  !-prefix means excludes.
	Patterns []string `json:"patterns,omitempty"`

	// glob pattern not to use as action inputs from inputs.
	Excludes []string `json:"excludes,omitempty"`

	// glob pattern to use as action inputs from inputs.
	Includes []string `json:"includes,omitempty"`

	// add other options? max depth etc?
}

// enabled returns true when PathFilter is enabled.
func (pf *PathFilter) enabled() bool {
	return pf != nil
}

// Filter returns filter function, which returns true when included, false when excluded, from PathFilter.
func (pf *PathFilter) Filter(ctx context.Context, name string) func(context.Context, string, bool) bool {
	patterns := pf.Patterns
	if len(patterns) == 0 {
		for _, p := range pf.Excludes {
			patterns = append(patterns, "!"+p)
		}
		if len(pf.Includes) == 0 {
			patterns = append(patterns, "*")
		} else {
			patterns = append(patterns, pf.Includes...)
		}
	}
	matchers := pathMatcher(ctx, name, patterns)
	return func(ctx context.Context, p string, debug bool) bool {
		for _, f := range matchers {
			ok, matched := f(ctx, p, debug)
			if matched {
				if debug {
					clog.Infof(ctx, "%s match %q => %t", name, p, ok)
				}
				return ok
			}
		}
		return false
	}
}

func pathMatcher(ctx context.Context, name string, pats []string) []func(context.Context, string, bool) (bool, bool) {
	var m []func(context.Context, string, bool) (bool, bool)
	for _, pat := range pats {
		pat, negative := strings.CutPrefix(pat, "!")
		op := "match"
		if negative {
			op = "no-match"
		}
		if pat == "*" {
			// match any file
			m = append(m, func(ctx context.Context, p string, debug bool) (bool, bool) {
				ok := !negative
				if debug {
					clog.Infof(ctx, "%s %s any: %q => %t", name, op, p, ok)
				}
				return ok, ok != negative
			})
			continue
		}
		if strings.HasPrefix(pat, "*") && !strings.ContainsAny(pat[1:], "*?[\\/") {
			// just has * prefix, and no pattern meta or '/' in suffix.
			// just suffix match with base name.
			suffix := pat[1:]
			m = append(m, func(ctx context.Context, p string, debug bool) (bool, bool) {
				ok := strings.HasSuffix(path.Base(p), suffix)
				if negative {
					ok = !ok
				}
				if debug {
					clog.Infof(ctx, "%s %s suffix %q: %q => %t", name, op, suffix, p, ok)
				}
				return ok, ok != negative
			})
			continue
		}
		pat, dirPrefix := strings.CutSuffix(pat, "/**")
		if dirPrefix {
			// prefix dir match
			// just check ErrBadPattern for pattern `prefix`.
			// it's sufficient to check error once, and no other way
			// to test pattern.
			_, err := path.Match(pat, pat)
			if err != nil {
				clog.Warningf(ctx, "bad %s pattern %q: %v", name, pat, err)
				continue
			}
			ndir := strings.Count(pat, "/")
			m = append(m, func(ctx context.Context, p string, debug bool) (bool, bool) {
				// use the same depth with dir prefix pattern.
				elems := strings.Split(p, "/")
				if len(elems) <= ndir {
					ok := false
					if negative {
						ok = true
					}
					if debug {
						clog.Infof(ctx, "%s %s prefix %q: %q => %t", name, op, pat, p, ok)
					}
					return ok, ok != negative
				}
				q := strings.Join(elems[:ndir+1], "/")
				ok, _ := path.Match(pat, q)
				if negative {
					ok = !ok
				}
				if debug {
					clog.Infof(ctx, "%s %s prefix %q: %q => %t", name, op, pat, p, ok)
				}
				return ok, ok != negative
			})
			continue
		}
		// just check ErrBadPattern for pattern `pat`.
		// it's sufficient to check error once, and no other way
		// to test pattern.
		_, err := path.Match(pat, pat)
		if err != nil {
			clog.Warningf(ctx, "bad %s pattern %q: %v", name, pat, err)
			continue
		}
		if strings.Count(pat, "/") == 0 {
			// basename match.
			m = append(m, func(ctx context.Context, p string, debug bool) (bool, bool) {
				b := path.Base(p)
				ok, _ := path.Match(pat, b)
				if negative {
					ok = !ok
				}
				if debug {
					clog.Infof(ctx, "%s %s pattern(base) %q: %q => %t", name, op, pat, p, ok)
				}
				return ok, ok != negative
			})
			continue
		}
		m = append(m, func(ctx context.Context, p string, debug bool) (bool, bool) {
			ok, _ := path.Match(pat, p)
			if negative {
				ok = !ok
			}
			if debug {
				clog.Infof(ctx, "%s %s pattern %q: %q => %t", name, op, pat, p, ok)
			}
			return ok, ok != negative
		})
	}
	return m
}
