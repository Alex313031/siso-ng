// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package pprof provides pprof supports.
package pprof

import (
	"compress/gzip"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	pb "go.chromium.org/build/siso/o11y/pprof/proto"
)

// Profile is a profile entry.
type Profile struct {
	name     string
	started  time.Time
	mu       sync.Mutex
	m        map[string][]time.Duration
	comments []string
}

// New creates new profile for the name.
func New(name string) *Profile {
	return &Profile{
		name:    name,
		started: time.Now(),
		m:       make(map[string][]time.Duration),
	}
}

// Name returns name of the profile.
func (p *Profile) Name() string {
	return p.name
}

// Duration returns duration of the profile.
func (p *Profile) Duration() time.Duration {
	return time.Since(p.started)
}

func (p *Profile) key(backtraces []string) string {
	return strings.Join(backtraces, "<")
}

// Add adds new duration for the backtraces.
func (p *Profile) Add(d time.Duration, backtraces []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := p.key(backtraces)
	p.m[key] = append(p.m[key], d)
}

// AddComment adds a comment to the profile.
func (p *Profile) AddComment(c string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.comments = append(p.comments, c)
}

// WriteTo writes profile to w,
// as https://pkg.go.dev/runtime/pprof#Profile.WriteTo
// not https://pkg.go.dev/io#WriterTo.
func (p *Profile) WriteTo(w io.Writer, debug int) error {
	if debug == 0 {
		return p.writeProtoTo(w)
	}
	return errors.New("not implemented")
}

func (p *Profile) writeProtoTo(w io.Writer) error {
	m := p.toProto()
	buf, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	gw := gzip.NewWriter(w)
	_, err = gw.Write(buf)
	if err != nil {
		gw.Close()
		return err
	}
	return gw.Close()
}

type profileBuilder struct {
	m *pb.Profile

	strtab map[string]int64
	loctab map[string]uint64
}

// profile data concepts
//  sample: location_id..., value..., label...
//  label: key, str or num, unit
//  location: id, line
//  line: function
//  function: id, name, system_name, [filename, start_line]

func (p *Profile) toProto() *pb.Profile {
	b := &profileBuilder{
		m:      &pb.Profile{},
		strtab: make(map[string]int64),
		loctab: make(map[string]uint64),
	}
	b.m.DurationNanos = time.Since(p.started).Nanoseconds()
	b.intern("")
	b.m.SampleType = append(b.m.SampleType, &pb.ValueType{
		Type: b.intern("samples"),
		Unit: b.intern("counts"),
	})
	b.m.SampleType = append(b.m.SampleType, &pb.ValueType{
		Type: b.intern("wall"),
		Unit: b.intern("milliseconds"),
	})

	p.mu.Lock()
	keys := make([]string, 0, len(p.m))
	for k := range p.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		locs := b.locations(strings.Split(k, "<"))
		ds := p.m[k]
		var totalD int64
		for _, d := range ds {
			totalD += d.Milliseconds()
		}
		b.m.Sample = append(b.m.Sample, &pb.Sample{
			LocationId: locs,
			Value: []int64{
				int64(len(ds)),
				totalD,
			},
		})
	}
	for _, c := range p.comments {
		b.m.Comment = append(b.m.Comment, b.intern(c))
	}
	p.mu.Unlock()
	return b.build()
}

func (b *profileBuilder) intern(s string) int64 {
	i, ok := b.strtab[s]
	if ok {
		return i
	}
	i = int64(len(b.m.StringTable))
	b.m.StringTable = append(b.m.StringTable, s)
	b.strtab[s] = i
	return i
}

func (b *profileBuilder) locations(backtrace []string) []uint64 {
	locs := make([]uint64, 0, len(backtrace))
	for _, bt := range backtrace {
		lid, ok := b.loctab[bt]
		if !ok {
			fnid := b.intern(bt)
			fid := uint64(len(b.m.Function) + 1)
			b.m.Function = append(b.m.Function, &pb.Function{
				Id:         fid,
				Name:       fnid,
				SystemName: fnid,
			})
			lid = uint64(len(b.m.Location) + 1)
			b.m.Location = append(b.m.Location, &pb.Location{
				Id: lid,
				Line: []*pb.Line{
					{
						FunctionId: fid,
					},
				},
			})
		}
		locs = append(locs, lid)
	}
	return locs
}

func (b *profileBuilder) build() *pb.Profile {
	b.m.TimeNanos = time.Now().UnixNano()
	return b.m
}
