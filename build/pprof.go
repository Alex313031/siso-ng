// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.chromium.org/build/siso/build/metadata"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/pprof"
	"go.chromium.org/build/siso/o11y/trace"
)

type tracePprof struct {
	fname string

	p *pprof.Profile
}

// newTracePprof creates new *tracePprof to record a file specified by fname.
// If fname is empty, *tracePprof does nothing.
func newTracePprof(fname string) *tracePprof {
	return &tracePprof{
		fname: fname,
		p:     pprof.New("build"),
	}
}

func (tp *tracePprof) Close(ctx context.Context) error {
	if tp.fname == "" {
		return nil
	}
	clog.Infof(ctx, "pprof write to %s", tp.fname)
	w, err := os.Create(tp.fname)
	if err != nil {
		return err
	}
	err = tp.p.WriteTo(w, 0)
	if err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func (tp *tracePprof) Add(ctx context.Context, tc *trace.Context) {
	if tp.fname == "" {
		return
	}
	spans := tc.Spans()
	if len(spans) == 0 {
		return
	}
	backtraces, _ := spans[0].Attrs[logLabelKeyBacktraces].([]string)

	for _, span := range spans[1:] {
		switch span.NameKind() {
		case "serv:preproc",
			"serv:localexec",
			"serv:remoteexec",
			"serv:remotecache",
			"serv:localcache-digest",
			"serv:remoteexec-digest":
			bt := append([]string{span.Name}, backtraces...)
			tp.p.Add(span.Duration(), bt)
		case "serv:deps-gcc", "serv:deps-msvc":
			bt := append([]string{span.Name, "serv:preproc"}, backtraces...)
			tp.p.Add(span.Duration(), bt)
		case "rbe:queue", "rbe:worker":
			bt := append([]string{span.Name, "serv:remoteexec"}, backtraces...)
			tp.p.Add(span.Duration(), bt)
		case "rbe:input", "rbe:exec", "rbe:output":
			bt := append([]string{span.Name, "rbe:worker", "serv:remoteexec"}, backtraces...)
			tp.p.Add(span.Duration(), bt)
		default:
			if strings.HasPrefix(span.Name, "pool=") {
				bt := append([]string{span.Name}, backtraces...)
				tp.p.Add(span.Duration(), bt)
			}
		}
	}
}

func (tp *tracePprof) SetMetadata(metadata metadata.Metadata) {
	if tp.fname == "" {
		return
	}
	for _, k := range metadata.SortedKeys() {
		tp.p.AddComment(fmt.Sprintf("%s=%q", k, metadata.Get(k)))
	}
}
