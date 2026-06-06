// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

// Node represents a node (target file) in build graph.
type Node struct {
	next *Node // for bigMap

	id   int
	path string

	inEdge atomic.Pointer[Edge] // the edge that generates the file for this node.

	nouts atomic.Int64

	outs []*Edge
}

func (n *Node) ID() int { return n.id }

func (n *Node) String() string { return n.path }

// Path is the path of the node.
func (n *Node) Path() string { return n.path }

// OutEdges returns out-edges of the node.
func (n *Node) OutEdges() []*Edge {
	return n.outs
}

func (n *Node) setInEdge(edge *Edge) bool {
	swapped := n.inEdge.CompareAndSwap(nil, edge)
	return swapped
}

// InEdge returns in-edge of the node.
func (n *Node) InEdge() (*Edge, bool) {
	e := n.inEdge.Load()
	return e, e != nil
}

// Edge represents an edge (action) in build graph.
// TODO(b/267409605): Add tests for Edge methods.
type Edge struct {
	rule *rule

	pool  *Pool
	pos   int
	env   buildScope
	scope *fileScope

	inputs      []*Node
	outputs     []*Node
	validations []*Node

	// https://ninja-build.org/manual.html#ref_dependencies
	implicitDeps  int
	orderOnlyDeps int
	// https://ninja-build.org/manual.html#ref_outputs
	implicitOuts int
}

type edgeEnv struct {
	edge        *Edge
	shellEscape bool
}

var lookupBuf = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		return buf
	},
}

// Lookup looks up a variable by key, and returns evaludated value.
func (e *edgeEnv) Lookup(key string) string {
	buf := lookupBuf.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		lookupBuf.Put(buf)
	}()
	s, err := e.lookup(buf, key)
	if err != nil {
		return ""
	}
	return string(s)
}

func (e *edgeEnv) lookup(buf *bytes.Buffer, key string) ([]byte, error) {
	val, ok := e.edge.env.lookupVar(e.edge.pos, []byte(key))
	if ok {
		s, err := evaluate(e, buf, val)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	// rule may be in parent scope
	val, ok = e.edge.rule.lookupVar(-1, []byte(key))
	if ok {
		val.pos = e.edge.pos
		s, err := evaluate(e, buf, val)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	if e.edge.scope == nil {
		return nil, nil
	}
	val, ok = e.edge.scope.lookupVar(e.edge.pos, []byte(key))
	if ok {
		s, err := evaluate(e, buf, val)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, nil
}

func (e *edgeEnv) lookupVar(pos int, key []byte) (evalString, bool) {
	sep := " "
	// Handle special in/out keys.
	// https://ninja-build.org/manual.html#ref_rule
	switch string(key) {
	case "in_newline":
		sep = "\n"
		fallthrough
	case "in":
		n := len(e.edge.inputs) - e.edge.implicitDeps - e.edge.orderOnlyDeps
		return evalString{
			v: []byte(e.pathList(e.edge.inputs[:n], sep)),
		}, true
	case "out":
		n := len(e.edge.outputs) - e.edge.implicitOuts
		return evalString{
			v: []byte(e.pathList(e.edge.outputs[:n], sep)),
		}, true
	}
	ev, ok := e.edge.env.lookupVar(pos, key)
	if ok {
		return ev, true
	}
	ev, ok = e.edge.rule.lookupVar(-1, key)
	if ok {
		ev.pos = e.edge.pos
		return ev, true
	}
	if e.edge.scope == nil {
		return evalString{}, false
	}
	return e.edge.scope.lookupVar(-1, key)
}

func (e *edgeEnv) pathList(paths []*Node, sep string) string {
	var s []string
	for _, n := range paths {
		p := n.path
		if e.shellEscape {
			p = shellEscape(p)
		}
		s = append(s, p)
	}
	return strings.Join(s, sep)
}

var cmdhashBuf = sync.Pool{
	New: func() any {
		// 8192 would be large enough for most command lines.
		buf := bytes.NewBuffer(make([]byte, 0, 8192))
		return buf
	},
}

const unitSeparator = "\x1f"

// CmdHash returns command hash of the edge.
func (e *Edge) CmdHash() []byte {
	buf := cmdhashBuf.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		cmdhashBuf.Put(buf)
	}()

	h := sha256.New()
	env := edgeEnv{edge: e, shellEscape: true}
	cmdline, err := env.lookup(buf, "command")
	if err == nil {
		h.Write(cmdline)
	}
	buf.Reset()
	rspfileContent, err := env.lookup(buf, "rspfile_content")
	if err == nil && len(rspfileContent) > 0 {
		h.Write([]byte(unitSeparator))
		h.Write(rspfileContent)
	}
	return h.Sum(nil)
}

// Binding returns binding value in the edge.
func (e *Edge) Binding(name string) string {
	env := edgeEnv{edge: e, shellEscape: true}
	return env.Lookup(name)
}

// BindingBool returns true if binding is defined in the edge.
func (e *Edge) BindingBool(name string) bool {
	return e.Binding(name) != ""
}

// UnescapedBinding returns binding value without shell escape.
func (e *Edge) UnescapedBinding(name string) string {
	env := edgeEnv{edge: e, shellEscape: false}
	return env.Lookup(name)
}

func (e *Edge) rawBinding(name []byte) ([]byte, bool) {
	val, ok := e.env.lookupVar(e.pos, name)
	if ok {
		return val.v, true
	}
	val, ok = e.rule.lookupVar(-1, name)
	if ok {
		return val.v, true
	}
	if e.scope == nil {
		return nil, false
	}
	val, ok = e.scope.lookupVar(e.pos, name)
	if ok {
		return val.v, true
	}
	return nil, false
}

// RawBinding returns raw eval string of binding value in the edge.
func (e *Edge) RawBinding(name string) string {
	env := edgeEnv{edge: e, shellEscape: true}
	sep := " "
	// Handle special in/out keys.
	// https://ninja-build.org/manual.html#ref_rule
	switch name {
	case "in_newline":
		sep = "\n"
		fallthrough
	case "in":
		n := len(env.edge.inputs) - env.edge.implicitDeps - env.edge.orderOnlyDeps
		return env.pathList(env.edge.inputs[:n], sep)
	case "out":
		n := len(env.edge.outputs) - env.edge.implicitOuts
		return env.pathList(env.edge.outputs[:n], sep)
	}
	val, ok := e.rawBinding([]byte(name))
	if ok {
		return string(val)
	}
	return ""
}

// RuleName returns a rule name used by the edge.
func (e *Edge) RuleName() string {
	return e.rule.Name()
}

// Pool returns a pool associated to the edge.
func (e *Edge) Pool() *Pool {
	return e.pool
}

// Inputs returns input nodes of the edge.
func (e *Edge) Inputs() []*Node {
	return e.inputs
}

// Ins returns explicit input nodes (for $in) of the edge.
func (e *Edge) Ins() []*Node {
	n := len(e.inputs) - e.implicitDeps - e.orderOnlyDeps
	return e.inputs[:n]
}

// TriggerInputs returns inputs nodes of the edge that would trigger
// the edge command. i.e. not including order_only inputs.
func (e *Edge) TriggerInputs() []*Node {
	return e.inputs[:len(e.inputs)-e.orderOnlyDeps]
}

// Outputs returns output nodes of the edge.
func (e *Edge) Outputs() []*Node {
	return e.outputs
}

// Validations returns validations node of the edge.
func (e *Edge) Validations() []*Node {
	return e.validations
}

// IsPhony returns true iff phony edge.
func (e *Edge) IsPhony() bool {
	return e.rule == phonyRule
}

// Print writes edge information in writer.
// TODO: add test for print.
func (e *Edge) Print(w io.Writer) {
	e.scope.Print(w)
	if e.pool != nil && e.pool.Name() != "" {
		fmt.Fprintf(w, "pool %s\n", e.pool.Name())
		fmt.Fprintf(w, "  depth = %d\n\n", e.pool.Depth())
	}
	fmt.Fprintf(w, "rule %s\n", e.rule.name)
	for _, rb := range e.rule.bindings {
		fmt.Fprintf(w, "  %s = %s\n", rb.name, rb.value.v)
	}
	fmt.Fprintf(w, "\n")
	if len(e.outputs) == 1 {
		fmt.Fprintf(w, "build %s $\n", escapeNinjaToken(e.outputs[0].Path()))
	} else {
		fmt.Fprintf(w, "build $\n")
		for i, n := range e.outputs {
			if i == len(e.outputs)-e.implicitOuts {
				fmt.Fprintf(w, "  | $\n")
			}
			switch {
			case i < len(e.outputs)-e.implicitOuts:
				fmt.Fprintf(w, "  %s $\n", escapeNinjaToken(n.Path()))
			default:
				fmt.Fprintf(w, "    %s $\n", escapeNinjaToken(n.Path()))
			}
		}
	}
	fmt.Fprintf(w, " : %s $\n", e.rule.Name())
	for i, n := range e.inputs {
		if i == len(e.inputs)-e.orderOnlyDeps-e.implicitDeps && e.implicitDeps > 0 {
			fmt.Fprintf(w, "  | $\n")
		}
		if i == len(e.inputs)-e.orderOnlyDeps {
			fmt.Fprintf(w, "  || $\n")
		}
		switch {
		case i < len(e.inputs)-e.orderOnlyDeps-e.implicitDeps:
			fmt.Fprintf(w, "  %s", escapeNinjaToken(n.Path()))
		case i < len(e.inputs)-e.orderOnlyDeps:
			fmt.Fprintf(w, "    %s", escapeNinjaToken(n.Path()))
		default:
			fmt.Fprintf(w, "      %s", escapeNinjaToken(n.Path()))
		}
		if i < len(e.inputs)-1 {
			fmt.Fprintf(w, " $\n")
		} else {
			fmt.Fprintf(w, "\n")
		}
	}
	if len(e.validations) > 0 {
		fmt.Fprintf(w, "  |@ $\n")
		for i, n := range e.validations {
			fmt.Fprintf(w, "  %s", escapeNinjaToken(n.Path()))
			if i < len(e.validations)-1 {
				fmt.Fprintf(w, " $\n")
			} else {
				fmt.Fprintf(w, "\n")
			}
		}
	}
	e.env.Print(w)
}

func escapeNinjaToken(s string) string {
	if strings.ContainsAny(s, " $:") {
		var sb strings.Builder
		for _, ch := range s {
			switch ch {
			case ' ', '$', ':':
				sb.WriteByte('$')
			}
			sb.WriteRune(ch)
		}
		return sb.String()
	}
	return s
}
