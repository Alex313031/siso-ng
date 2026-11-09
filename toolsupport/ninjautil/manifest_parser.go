// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sync/errgroup"

	"go.chromium.org/build/siso/sync/semaphore"
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

// maxLoaderConcurrency prevents Go GC scaling stalls when hundreds of
// parsers allocate from a large graph at once.
const maxLoaderConcurrency = 128

var loaderConcurrency = min(runtime.GOMAXPROCS(0), maxLoaderConcurrency)

// SetWd sets working directory to use for loading files.
func (p *ManifestParser) SetWd(wd string) {
	p.wd = wd
}

// Load loads the Ninja manifest given an fname. Manifest mappings are
// released before Load returns, on success or failure.
func (p *ManifestParser) Load(ctx context.Context, fname string) (retErr error) {
	p.fsema = make(chan struct{}, loaderConcurrency)
	defer func() {
		if cerr := p.state.Close(); cerr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release manifest mmaps: %w", cerr))
		}
	}()
	if err := p.loadFiles(ctx, fname); err != nil {
		return err
	}
	var err error
	p.state.nodes, p.state.dirOutputs, err = p.state.nodeMap.freeze(ctx)
	return err
}

// loadFiles loads fname and its subninjas, parsing at most
// loaderConcurrency files at once. A file's goroutine starts only after it
// acquired a permit, so files waiting for one don't hold a goroutine each.
func (p *ManifestParser) loadFiles(ctx context.Context, fname string) error {
	eg, gctx := errgroup.WithContext(ctx)
	sema := semaphore.New("ninja-load", loaderConcurrency)
	var spawn func(fname string, scope *fileScope) error
	spawn = func(fname string, scope *fileScope) error {
		sctx, release, err := sema.WaitAcquire(gctx)
		if err != nil {
			return err
		}
		eg.Go(func() error {
			subninjas, err := p.loadFile(sctx, fname, scope)
			// Release the permit before waiting for the subninjas' permits,
			// or parents holding every permit would wait for each other.
			release(err)
			if err != nil {
				return err
			}
			for _, sub := range subninjas {
				if err := spawn(sub, newFileScope(scope)); err != nil {
					return err
				}
			}
			return nil
		})
		return nil
	}
	if err := spawn(fname, p.scope); err != nil {
		return err
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	// The parser doesn't check ctx, so a cancellation during the last parses
	// would go unnoticed otherwise.
	return context.Cause(ctx)
}

func (p *ManifestParser) loadFile(ctx context.Context, fname string, scope *fileScope) ([]string, error) {
	fp := &fileParser{
		scope: scope,
		state: p.state,
		sema:  p.fsema,
		wd:    p.wd,
	}
	err := fp.parseFile(ctx, fname)
	if err != nil {
		return nil, err
	}
	return fp.fileState.subninjas, nil
}

// LoadSingle loads the Ninja manifest given an fname, but not loads subninjas.
func (p *ManifestParser) LoadSingle(ctx context.Context, fname string) (retErr error) {
	p.fsema = make(chan struct{}, loaderConcurrency)
	defer func() {
		if cerr := p.state.Close(); cerr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release manifest mmaps: %w", cerr))
		}
	}()
	fp := &fileParser{
		scope: p.scope,
		state: p.state,
		sema:  p.fsema,
	}
	if err := fp.parseFile(ctx, fname); err != nil {
		return err
	}
	var err error
	p.state.nodes, p.state.dirOutputs, err = p.state.nodeMap.freeze(ctx)
	return err
}
