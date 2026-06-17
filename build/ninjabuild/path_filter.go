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
type PathFilter struct {
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

func (pf *PathFilter) filter(ctx context.Context, name string) func(context.Context, string, bool) bool {
	excludes := pathMatcher(ctx, name, pf.Excludes)
	includes := pathMatcher(ctx, name, pf.Includes)
	return func(ctx context.Context, p string, debug bool) bool {
		for _, f := range excludes {
			if f(ctx, p, debug) {
				if debug {
					clog.Infof(ctx, "%s excludes %q", name, p)
				}
				return false
			}
		}
		if len(includes) == 0 {
			if debug {
				clog.Infof(ctx, "%s include-by-default %q", name, p)
			}
			return true
		}
		for _, f := range includes {
			if f(ctx, p, debug) {
				return true
			}
		}
		if debug {
			clog.Infof(ctx, "%s match none %q", name, p)
		}
		return false
	}
}

func pathMatcher(ctx context.Context, name string, pats []string) []func(context.Context, string, bool) bool {
	var m []func(context.Context, string, bool) bool
	for _, pat := range pats {
		if pat == "*" {
			// match any file
			m = append(m, func(ctx context.Context, p string, debug bool) bool {
				if debug {
					clog.Infof(ctx, "%s match any: %q", name, p)
				}
				return true
			})
			continue
		}
		if strings.HasPrefix(pat, "*") && !strings.ContainsAny(pat[1:], "*?[\\/") {
			// just has * prefix, and no pattern meta or '/' in suffix.
			// just suffix match with base name.
			suffix := pat[1:]
			m = append(m, func(ctx context.Context, p string, debug bool) bool {
				ok := strings.HasSuffix(path.Base(p), suffix)
				if debug {
					clog.Infof(ctx, "%s match suffix %q: %q => %t", name, suffix, p, ok)
				}
				return ok
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
			m = append(m, func(ctx context.Context, p string, debug bool) bool {
				b := path.Base(p)
				ok, _ := path.Match(pat, b)
				if debug {
					clog.Infof(ctx, "%s match pattern(base) %q: %q => %t", name, pat, p, ok)
				}
				return ok
			})
			continue
		}
		m = append(m, func(ctx context.Context, p string, debug bool) bool {
			ok, _ := path.Match(pat, p)
			if debug {
				clog.Infof(ctx, "%s match pattern %q: %q => %t", name, pat, p, ok)
			}
			return ok
		})
	}
	return m
}
