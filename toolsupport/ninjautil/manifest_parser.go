// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"fmt"
	"path/filepath"

	"golang.org/x/sync/errgroup"

	"go.chromium.org/build/siso/runtimex"
)

// multipleRulesError is an error that multiple rules generates the same output.
type multipleRulesError struct {
	target string
}

func (e multipleRulesError) Error() string {
	return fmt.Sprintf("multiple rules generates %s", e.target)
}

// ManifestParser parses Ninja manifests. (i.e. .ninja files)
type ManifestParser struct {
	// Stores all information found while parsing the .ninja file.
	state *State
	scope *fileScope

	eg    *errgroup.Group
	sema  chan struct{}
	fsema chan struct{}
	wd    string
}

// NewManifestParser creates a new manifest parser.
func NewManifestParser(state *State) *ManifestParser {
	return &ManifestParser{
		state: state,
		scope: state.scope,
	}
}

var loaderConcurrency = runtimex.NumCPU()

// SetWd sets working directory to use for loading files.
func (p *ManifestParser) SetWd(wd string) {
	p.wd = wd
}

// Load loads the Ninja manifest given an fname.
func (p *ManifestParser) Load(ctx context.Context, fname string) error {
	if p.eg == nil {
		p.eg, ctx = errgroup.WithContext(ctx)
		p.sema = make(chan struct{}, loaderConcurrency)
		p.fsema = make(chan struct{}, loaderConcurrency)
	}
	p.eg.Go(func() error {
		return p.loadFile(ctx, fname)
	})
	err := p.eg.Wait()
	if err != nil {
		return err
	}
	p.state.nodes = p.state.nodeMap.freeze(ctx)
	for _, edge := range p.state.edges {
		for _, in := range edge.inputs {
			if in.outs == nil {
				in.outs = make([]*Edge, 0, in.nouts.Load())
			}
			in.outs = append(in.outs, edge)
		}
	}
	return nil
}

func (p *ManifestParser) loadFile(ctx context.Context, fname string) error {
	fp := &fileParser{
		scope: p.scope,
		state: p.state,
		sema:  p.fsema,
	}
	err := fp.parseFile(ctx, filepath.Join(p.wd, fname))
	if err != nil {
		return err
	}
	for _, fname := range fp.fileState.subninjas {
		scope := newFileScope(fp.scope)
		p.eg.Go(func() error {
			p.sema <- struct{}{}
			defer func() { <-p.sema }()
			subparser := &ManifestParser{
				state: p.state,
				scope: scope,
				eg:    p.eg,
				sema:  p.sema,
				fsema: p.fsema,
				wd:    p.wd,
			}
			return subparser.loadFile(ctx, fname)
		})
	}
	return nil
}

// LoadSingle loads the Ninja manifest given an fname, but not loads subninjas.
func (p *ManifestParser) LoadSingle(ctx context.Context, fname string) error {
	if p.eg == nil {
		p.eg, ctx = errgroup.WithContext(ctx)
		p.sema = make(chan struct{}, loaderConcurrency)
		p.fsema = make(chan struct{}, loaderConcurrency)
	}
	p.eg.Go(func() error {
		fp := &fileParser{
			scope: p.scope,
			state: p.state,
			sema:  p.fsema,
		}
		return fp.parseFile(ctx, filepath.Join(p.wd, fname))
	})
	err := p.eg.Wait()
	if err != nil {
		return err
	}
	p.state.nodes = p.state.nodeMap.freeze(ctx)
	for _, edge := range p.state.edges {
		for _, in := range edge.inputs {
			if in.outs == nil {
				in.outs = make([]*Edge, 0, in.nouts.Load())
			}
			in.outs = append(in.outs, edge)
		}
	}
	return nil
}
