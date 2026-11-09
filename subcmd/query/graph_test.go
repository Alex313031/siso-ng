// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"bytes"
	"flag"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestQueryGraph(t *testing.T) {
	for _, tc := range []struct {
		name       string
		buildNinja string
		want       string
	}{
		{
			name: "simple_graph",
			buildNinja: `
rule cxx
  command = clang++ -c $in -o $out
  depfile = $out.d
  deps = gcc

build obj/foo/bar.o: cxx src/foo/bar.cc
`,
			want: `{"output":"obj/foo/bar.o","rule":"cxx","inputs":["src/foo/bar.cc"],"command":"clang++ -c src/foo/bar.cc -o obj/foo/bar.o","depfile":"obj/foo/bar.o.d","deps":"gcc"}
`,
		},
		{
			name: "implicit_and_order_only_and_validation_and_extra_outputs",
			buildNinja: `
rule myrule
  command = mycmd $in -o $out

build out.o extra_b.cc extra_a.cc | imp_b.h imp_a.h: myrule in_b.c in_a.c | imp_dep_b.h imp_dep_a.h || order_b.stamp order_a.stamp |@ val_b.stamp val_a.stamp
`,
			want: `{"output":"out.o","rule":"myrule","extra_outputs":["extra_a.cc","extra_b.cc"],"implicit_outputs":["imp_a.h","imp_b.h"],"inputs":["in_a.c","in_b.c"],"implicit_inputs":["imp_dep_a.h","imp_dep_b.h"],"order_only_inputs":["order_a.stamp","order_b.stamp"],"validation_inputs":["val_a.stamp","val_b.stamp"],"command":"mycmd in_b.c in_a.c -o out.o extra_b.cc extra_a.cc"}
`,
		},
		{
			name: "variable_hoisting_semantic_equivalence",
			buildNinja: `
cxx_compiler = clang++

rule cxx
  command = $cxx_compiler -c $in -o $out

build obj/base.o: cxx src/base.cc
`,
			want: `{"output":"obj/base.o","rule":"cxx","inputs":["src/base.cc"],"command":"clang++ -c src/base.cc -o obj/base.o"}
`,
		},
		{
			name: "multiline_command_and_rspfile",
			buildNinja: `
rule link
  command = ld @$out.rsp -o $out
  rspfile = $out.rsp
  rspfile_content = $in_newline

build bin/app: link obj/second.o obj/first.o
`,
			want: `{"output":"bin/app","rule":"link","inputs":["obj/first.o","obj/second.o"],"command":"ld @bin/app.rsp -o bin/app","rspfile":"bin/app.rsp","rspfile_content":"obj/second.o\nobj/first.o"}
`,
		},
		{
			name: "multiple_edges",
			buildNinja: `
rule cxx
  command = clang++ -c $in -o $out

build obj/beta.o: cxx src/beta.cc
build obj/alpha.o: cxx src/alpha.cc
`,
			want: `{"output":"obj/alpha.o","rule":"cxx","inputs":["src/alpha.cc"],"command":"clang++ -c src/alpha.cc -o obj/alpha.o"}
{"output":"obj/beta.o","rule":"cxx","inputs":["src/beta.cc"],"command":"clang++ -c src/beta.cc -o obj/beta.o"}
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			err := os.MkdirAll("out/siso", 0755)
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir("out/siso")
			err = os.WriteFile("build.ninja", []byte(tc.buildNinja), 0644)
			if err != nil {
				t.Fatal(err)
			}

			var buf bytes.Buffer
			c := &graphCommand{w: &buf}
			flagSet := flag.NewFlagSet("graph", flag.ContinueOnError)
			c.SetFlags(flagSet)
			err = flagSet.Parse(nil)
			if err != nil {
				t.Fatal(err)
			}
			err = c.run(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			got := buf.String()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("query graph output diff -want +got:\n%s", diff)
			}
		})
	}
}
