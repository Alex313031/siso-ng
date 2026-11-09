// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestSplitIntoChunksCapacity(t *testing.T) {
	old := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(old)

	chunks := splitIntoChunks(t.Context(), []byte("rule cc\n"))
	if got, want := cap(chunks), 1; got != want {
		t.Errorf("cap(splitIntoChunks(small manifest)) = %d, want %d", got, want)
	}
}

// TestParseChunk_TrailingComment checks that a comment on the last line
// without a newline ends the chunk instead of looping forever.
func TestParseChunk_TrailingComment(t *testing.T) {
	ch := splitIntoChunks(t.Context(), []byte("rule cc\n  command = cc\n# trailing"))[0]
	done := make(chan error, 1)
	go func() { done <- ch.parseChunk(t.Context()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("parseChunk didn't return")
	}
	if got, want := ch.ncomment, 1; got != want {
		t.Errorf("ncomment = %d, want %d", got, want)
	}
	if got, want := len(ch.statements), 2; got != want {
		t.Errorf("len(statements) = %d, want %d", got, want)
	}
}

func TestParseChunk_AllocCounts(t *testing.T) {
	manifest := []byte(`v1 = a
v2 = b
pool p
  depth = 1
rule cc
  command = cc $in -o $out
  description = CC $out
build out: cc in
  b1 = 1
  b2 = 2
  b3 = 3
  b4 = 4
  b5 = 5
  b6 = 6
  b7 = 7
  b8 = 8
`)
	fp := &fileParser{
		state: NewState(),
		scope: newFileScope(nil),
		sema:  make(chan struct{}, 1),
		buf:   manifest,
	}
	fp.chunks = splitIntoChunks(t.Context(), fp.buf)
	if err := fp.parseChunks(t.Context()); err != nil {
		t.Fatalf("parseChunks: %v", err)
	}
	fp.alloc(t.Context())

	if got, want := fp.full.nvar, 2; got != want {
		t.Errorf("fp.full.nvar = %d, want %d", got, want)
	}
	if got, want := fp.full.nrulevar, 2; got != want {
		t.Errorf("fp.full.nrulevar = %d, want %d", got, want)
	}
	if got, want := fp.full.nbuildvar, 8; got != want {
		t.Errorf("fp.full.nbuildvar = %d, want %d", got, want)
	}
	if got, want := fp.full.npoolvar, 1; got != want {
		t.Errorf("fp.full.npoolvar = %d, want %d", got, want)
	}
	if got, want := len(fp.bindingArena.allocs), 2; got != want {
		t.Errorf("len(fp.bindingArena.allocs) = %d, want %d", got, want)
	}
	if got, want := len(fp.chunks[0].bindingArena.allocs), 2; got != want {
		t.Errorf("len(fp.chunks[0].bindingArena.allocs) = %d, want %d", got, want)
	}
	if got, want := len(fp.scope.bindings.shards), 1; got != want {
		t.Errorf("len(fp.scope.bindings.shards) = %d, want %d", got, want)
	}
}

// TestParseChunk_TrailingKeyword checks that a keyword at the end of the
// buffer is an error instead of a panic.
func TestParseChunk_TrailingKeyword(t *testing.T) {
	for _, kw := range []string{"build", "default", "include", "pool", "rule", "subninja"} {
		t.Run(kw, func(t *testing.T) {
			ch := splitIntoChunks(t.Context(), []byte("rule cc\n  command = cc\n"+kw))[0]
			if err := ch.parseChunk(t.Context()); err == nil {
				t.Errorf("parseChunk: got statements %v; want error", ch.statements)
			}
		})
	}
}

// TestParseChunk_IndentedBindingCounts checks that comments and
// whitespace-only lines between bindings do not contribute to the pool,
// rule, or build binding counts.
func TestParseChunk_IndentedBindingCounts(t *testing.T) {
	const manifest = `pool p
  depth = 1
rule cc
  command = cc $in -o $out
  description = CC $out
build out: cc in
  b1 = 1
  b2 = 2
  b3 = 3
`
	for _, tc := range []struct {
		name    string
		skipped string
	}{
		{name: "bindings_only"},
		{name: "comments", skipped: "  # ignored\n"},
		{name: "whitespace", skipped: "  \n"},
		{name: "comments_and_whitespace", skipped: "  # ignored\n  \n"},
		{name: "top_level_comments", skipped: "# ignored\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, newline := range []string{"\n", "\r\n"} {
				name := "LF"
				if newline == "\r\n" {
					name = "CRLF"
				}
				t.Run(name, func(t *testing.T) {
					// Add skipped lines before and after each binding.
					var buf strings.Builder
					for line := range strings.SplitSeq(manifest, "\n") {
						if strings.HasPrefix(line, "  ") {
							buf.WriteString(tc.skipped)
							buf.WriteString(line + "\n")
							buf.WriteString(tc.skipped)
						} else {
							buf.WriteString(line + "\n")
						}
					}
					ch := splitIntoChunks(t.Context(), []byte(strings.ReplaceAll(buf.String(), "\n", newline)))[0]
					if err := ch.parseChunk(t.Context()); err != nil {
						t.Fatal(err)
					}
					if got, want := ch.npoolvar, 1; got != want {
						t.Errorf("npoolvar = %d, want %d", got, want)
					}
					if got, want := ch.nrulevar, 2; got != want {
						t.Errorf("nrulevar = %d, want %d", got, want)
					}
					if got, want := ch.nbuildvar, 3; got != want {
						t.Errorf("nbuildvar = %d, want %d", got, want)
					}
					if got, want := len(ch.statements), 9; got != want {
						t.Errorf("len(statements) = %d, want %d", got, want)
					}
				})
			}
		})
	}
}

// TestCompactBuildStatements_Empty verifies the (nil, nil) early return.
func TestCompactBuildStatements_Empty(t *testing.T) {
	ch := &chunk{}
	gotBuf, gotStmts := ch.compactBuildStatements([]byte("anything"), nil)
	if gotBuf != nil {
		t.Errorf("buf = %q; want nil", gotBuf)
	}
	if gotStmts != nil {
		t.Errorf("stmts = %v; want nil", gotStmts)
	}
}

// TestCompactBuildStatements_Single covers the single-statement case:
// the byte range from s..e is copied and offsets are rebased to 0.
func TestCompactBuildStatements_Single(t *testing.T) {
	// Manifest layout (positions chosen to make the rebase obvious):
	//   <100 bytes of unrelated prefix><cflags = -O2 -DFOO\n>
	prefix := make([]byte, 100)
	line := []byte("  cflags = -O2 -DFOO\n")
	buf := append(prefix, line...)

	in := []statement{{
		pos: 42,
		t:   statementBuildVar,
		s:   100, // start of "  cflags..."
		v:   109, // position of '='
		e:   100 + len(line),
	}}
	ch := &chunk{}
	gotBuf, gotStmts := ch.compactBuildStatements(buf, in)

	if string(gotBuf) != string(line) {
		t.Errorf("buf = %q; want %q", gotBuf, line)
	}
	want := []statement{{
		pos: 42,
		t:   statementBuildVar,
		s:   0,
		v:   9,
		e:   len(line),
	}}
	if diff := cmp.Diff(want, gotStmts, cmp.AllowUnexported(statement{})); diff != "" {
		t.Errorf("stmts mismatch (-want +got):\n%s", diff)
	}
}

// TestCompactBuildStatements_Multiple covers multiple statements: the
// returned byte range spans first.s to last.e, every statement's offsets
// are rebased relative to first.s, and pos / type are preserved.
func TestCompactBuildStatements_Multiple(t *testing.T) {
	prefix := make([]byte, 1000)
	region := []byte("  cflags = -O2\n  cppflags = -I/usr/include\n  ldflags = -lm\n")
	buf := append(prefix, region...)

	// statements[i].s/v/e are absolute offsets into buf.
	mk := func(pos int, sOff, vOff, eOff int) statement {
		return statement{
			pos: pos,
			t:   statementBuildVar,
			s:   1000 + sOff,
			v:   1000 + vOff,
			e:   1000 + eOff,
		}
	}
	in := []statement{
		mk(10, 0, 9, 15),            // "  cflags = -O2\n"
		mk(11, 15, 26, 43),          // "  cppflags = -I/usr/include\n"
		mk(12, 43, 53, len(region)), // "  ldflags = -lm\n"
	}
	ch := &chunk{}
	gotBuf, gotStmts := ch.compactBuildStatements(buf, in)

	if string(gotBuf) != string(region) {
		t.Errorf("buf = %q; want %q", gotBuf, region)
	}
	want := []statement{
		{pos: 10, t: statementBuildVar, s: 0, v: 9, e: 15},
		{pos: 11, t: statementBuildVar, s: 15, v: 26, e: 43},
		{pos: 12, t: statementBuildVar, s: 43, v: 53, e: len(region)},
	}
	if diff := cmp.Diff(want, gotStmts, cmp.AllowUnexported(statement{})); diff != "" {
		t.Errorf("stmts mismatch (-want +got):\n%s", diff)
	}

	// Spot-check that each statement's offsets index into gotBuf in a way
	// that recovers the original payload (the same payload they pointed to
	// in buf).
	for i, st := range gotStmts {
		gotSlice := gotBuf[st.s:st.e]
		wantSlice := buf[in[i].s:in[i].e]
		if string(gotSlice) != string(wantSlice) {
			t.Errorf("stmt %d slice = %q; want %q", i, gotSlice, wantSlice)
		}
	}
}

// TestCompactBuildStatements_BufferIndependent ensures the returned buf
// no longer aliases the input. This is the property State.Close depends
// on: after compact, the source mmap may be released.
func TestCompactBuildStatements_BufferIndependent(t *testing.T) {
	src := []byte("  key = value\n")
	in := []statement{{
		pos: 1,
		t:   statementBuildVar,
		s:   0,
		v:   6,
		e:   len(src),
	}}
	ch := &chunk{}
	gotBuf, _ := ch.compactBuildStatements(src, in)

	// Mutate the source; the returned buf must be unchanged.
	for i := range src {
		src[i] = 0
	}
	if want := "  key = value\n"; string(gotBuf) != want {
		t.Errorf("after src mutation, gotBuf = %q; want %q", gotBuf, want)
	}
}

// TestCompactBuildStatements_ArenaReuse verifies that consecutive calls
// share the same arena block when there is room: the second call's bytes
// land contiguously after the first call's bytes.
func TestCompactBuildStatements_ArenaReuse(t *testing.T) {
	ch := &chunk{
		freezeBytesArena: bumpArena[byte]{initialBlock: byteArenaInitialBlock, maxBlock: byteArenaMaxBlock},
		freezeStmtsArena: bumpArena[statement]{initialBlock: statementArenaInitialBlock, maxBlock: statementArenaMaxBlock},
	}
	stmt := []statement{{t: statementBuildVar, s: 0, v: 0, e: 4}}
	out1, _ := ch.compactBuildStatements([]byte("aaaa"), stmt)
	out2, _ := ch.compactBuildStatements([]byte("bbbb"), stmt)

	if got := len(ch.freezeBytesArena.blocks); got != 1 {
		t.Errorf("freezeBytesArena blocks = %d; want 1 (arena should reuse)", got)
	}
	if string(out1) != "aaaa" || string(out2) != "bbbb" {
		t.Errorf("outputs corrupted: out1=%q out2=%q", out1, out2)
	}
}
