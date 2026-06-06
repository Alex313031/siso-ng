// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestState_Targets(t *testing.T) {
	dir := t.TempDir()
	dir = filepath.Join(dir, "out/siso")
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(dir)

	err = os.MkdirAll(filepath.Join(dir, "../../foo"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "../../foo/foo.cc"), []byte(`
#include "foo/foo.h"
#include "foo/foo_util.h"
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "../../foo/foo.h"), nil, 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "../../foo/foo_util.h"), nil, 0644)
	if err != nil {
		t.Fatal(err)
	}

	inputNoDefault := `
rule cxx
  command = clang++ -c ${in} ${out}

build obj/foo.o: cxx ../../foo/foo.cc

build foo: phony obj/foo.o
build all: phony foo
build full: phony foo obj/foo.o
`

	input := inputNoDefault + `
default all
`
	for _, tc := range []struct {
		name    string
		input   string
		args    []string
		want    []string
		wantErr bool
	}{
		{
			name:  "no_default",
			input: inputNoDefault,
			args:  nil,
			want:  []string{"all", "full"},
		},
		{
			name:  "default",
			input: input,
			args:  nil,
			want:  []string{"all"},
		},
		{
			name:  "all_given",
			input: input,
			args:  []string{"all"},
			want:  []string{"all"},
		},
		{
			name:  "foo_given",
			input: input,
			args:  []string{"foo"},
			want:  []string{"foo"},
		},
		{
			name:  "dotslash",
			input: input,
			args:  []string{"./foo"},
			want:  []string{"foo"},
		},
		{
			name:  "dotslash_extra",
			input: input,
			args:  []string{".////obj////foo.o"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:    "wrong_target",
			input:   input,
			args:    []string{"bar"},
			wantErr: true,
		},
		{
			name:  "special^",
			input: input,
			args:  []string{"../../foo/foo.cc^"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:    "missing^",
			input:   input,
			args:    []string{"../../foo/bar.cc^"},
			wantErr: true,
		},
		{
			name:  "header^",
			input: input,
			args:  []string{"../../foo/foo.h^"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:  "dotslash^",
			input: input,
			args:  []string{"./../../foo/foo.h^"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:  "dotslash_extra^",
			input: input,
			args:  []string{"./////..///////../foo/foo.h^"},
			want:  []string{"obj/foo.o"},
		},
		{
			name:    "missing-header^",
			input:   input,
			args:    []string{"../../foo/bar.h^"},
			wantErr: true,
		},
		{
			name:  "header^-include",
			input: input,
			args:  []string{"../../foo/foo_util.h^"},
			want:  []string{"obj/foo.o"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			err := os.WriteFile("input", []byte(tc.input), 0644)
			if err != nil {
				t.Fatal(err)
			}
			state := NewState()
			p := NewManifestParser(state)
			err = p.Load(ctx, "input")
			if err != nil {
				t.Fatalf("parse %v", err)
			}
			nodes, err := state.Targets(tc.args)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("state.Targets(%q)=%q, %v; want %q; err=%t", tc.args, nodes, err, tc.want, tc.wantErr)
			}
			var got []string
			for _, n := range nodes {
				got = append(got, n.Path())
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("state.Targets(%q) diff -want +got:\n%s", tc.args, diff)
			}
		})
	}
}

// TestState_PostCloseAccess verifies that every reader on Edge / fileScope
// returns the same data after the manifest mappings are released, and that
// a redundant explicit Close is idempotent.
func TestState_PostCloseAccess(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	// Cover the paths that hold byte slices into the manifest:
	//   * top-level fileScope vars (cflags)
	//   * rule bindings (command, description, rspfile, rspfile_content)
	//   * per-edge build vars including a recursive $cflags reference
	//   * subninja with its own scope chain and edge bindings
	//   * a custom pool referenced from an edge
	main := `
cflags = -O2 -Wall

pool slow
  depth = 1

rule cc
  command = clang $cflags -c $in -o $out
  description = CC $out
  rspfile = $out.rsp
  rspfile_content = $cflags $in

build out/foo.o: cc src/foo.cc
  cflags = $cflags -DFEATURE
build out/bar.o: cc src/bar.cc
  pool = slow

subninja sub.ninja
`
	sub := `
rule link
  command = clang++ $ldflags $in -o $out
  description = LINK $out

ldflags = -lc

build out/prog: link out/foo.o out/bar.o
  ldflags = $ldflags -lm
`
	if err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(main), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub.ninja"), []byte(sub), 0644); err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	if err := p.Load(ctx, "build.ninja"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	type Snap struct {
		Path         string
		Rule         string
		Pool         string
		Cmd          string
		Desc         string
		RawCmd       string
		RawDesc      string
		RawRsp       string
		RawRspfile   string
		UnescapedCmd string
		CmdHash      []byte
		PrintOut     string
	}
	type edgeRef struct {
		path string
		edge *Edge
	}
	var edges []edgeRef
	for _, n := range state.AllNodes() {
		e, ok := n.InEdge()
		if !ok || e.IsPhony() {
			continue
		}
		edges = append(edges, edgeRef{path: n.Path(), edge: e})
	}
	if len(edges) != 3 {
		t.Fatalf("expected 3 edges, got %d", len(edges))
	}

	snapshot := func() []Snap {
		out := make([]Snap, 0, len(edges))
		for _, er := range edges {
			pool := ""
			if p := er.edge.Pool(); p != nil {
				pool = p.Name()
			}
			var pb bytes.Buffer
			er.edge.Print(&pb)
			out = append(out, Snap{
				Path:         er.path,
				Rule:         er.edge.RuleName(),
				Pool:         pool,
				Cmd:          er.edge.Binding("command"),
				Desc:         er.edge.Binding("description"),
				RawCmd:       er.edge.RawBinding("command"),
				RawDesc:      er.edge.RawBinding("description"),
				RawRsp:       er.edge.RawBinding("rspfile_content"),
				RawRspfile:   er.edge.RawBinding("rspfile"),
				UnescapedCmd: er.edge.UnescapedBinding("command"),
				CmdHash:      append([]byte(nil), er.edge.CmdHash()...),
				PrintOut:     pb.String(),
			})
		}
		return out
	}

	before := snapshot()
	if err := state.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := snapshot()

	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("edge readouts changed after Close (-before +after):\n%s", diff)
	}

	// Sanity-check the values themselves so a regression that returns
	// stable-but-wrong data also fails.
	wantCmd := map[string]string{
		"out/foo.o": "clang -O2 -Wall -DFEATURE -c src/foo.cc -o out/foo.o",
		"out/bar.o": "clang -O2 -Wall -c src/bar.cc -o out/bar.o",
		"out/prog":  "clang++ -lc -lm out/foo.o out/bar.o -o out/prog",
	}
	for _, s := range after {
		if got, want := s.Cmd, wantCmd[s.Path]; got != want {
			t.Errorf("%s command after Close = %q; want %q", s.Path, got, want)
		}
	}

	// Top-level binding lookup also reads from the (now frozen) shardBindings.
	if got := state.Binding("cflags"); got != "-O2 -Wall" {
		t.Errorf("state.Binding(cflags) after Close = %q; want %q", got, "-O2 -Wall")
	}

	// Close is idempotent; calling it again must not error or crash.
	if err := state.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
