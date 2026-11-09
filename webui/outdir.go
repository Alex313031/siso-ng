// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.chromium.org/build/siso/toolsupport/ninjautil"
	"go.chromium.org/build/siso/webui/invocation"
)

// outdirProvider manages metrics for a set of outdirs relative to a given workspace root.
type outdirProvider struct {
	workspaceRoot   string
	defaultManifest string
	defaultOutdir   string

	mu            sync.Mutex
	outdirMetrics map[string]*outdirInfo
}

func makeOutdirProvider(workspaceRoot, defaultManifest, defaultOutdir string) outdirProvider {
	return outdirProvider{
		workspaceRoot:   workspaceRoot,
		defaultManifest: defaultManifest,
		defaultOutdir:   defaultOutdir,
		outdirMetrics:   make(map[string]*outdirInfo),
	}
}

// Get lazy-loads outdir at a given path, returning cached result if possible.
func (r *outdirProvider) Get(outdir string) (invocation.Series[*buildMetrics], error) {
	abs := filepath.Join(r.workspaceRoot, outdir)
	r.mu.Lock()
	outdirInfo, ok := r.outdirMetrics[abs]
	defer r.mu.Unlock()
	if !ok {
		// For output directories with custom manifest paths (e.g. flat output directories),
		// use the manifest path specified by -f instead of falling back to the hardcoded "build.ninja".
		// TODO: refactor to generic manifest path resolution logic?
		manifestPath := "build.ninja"
		if abs == filepath.Join(r.workspaceRoot, r.defaultOutdir) {
			manifestPath = r.defaultManifest
		}
		var err error
		outdirInfo, err = loadOutdirInfo(r.workspaceRoot, abs, manifestPath)
		if err != nil {
			return nil, fmt.Errorf("couldn't load outdir %s: %w", abs, err)
		}
		r.outdirMetrics[abs] = outdirInfo
	}
	return outdirInfo, nil
}

// Invalidate drops information about the given outdir.
func (r *outdirProvider) Invalidate(outdir string) {
	abs := filepath.Join(r.workspaceRoot, outdir)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.outdirMetrics, abs)
}

type outdirInfo struct {
	path         string
	pathRel      string
	manifestPath string
	metrics      []*buildMetrics
	latestRevID  string

	mu            sync.Mutex
	manifestMtime time.Time
	ninjaState    *ninjautil.State
}

// Get returns the outdir that matches the provided rev.
// TODO: rename rev to id?
func (i *outdirInfo) Get(rev string) *buildMetrics {
	for _, m := range i.metrics {
		if m.ID() == rev {
			return m
		}
	}
	return nil
}

// Latest returns the most recent build revision found in this outdir.
func (i *outdirInfo) Latest() *buildMetrics {
	return i.Get(i.latestRevID)
}

// All returns an iterator over all build revisions found in this outdir.
func (i *outdirInfo) All() iter.Seq[*buildMetrics] {
	return slices.Values(i.metrics)
}

// Title returns the display title for this outdir.
func (i *outdirInfo) Title() string {
	// Try to replace the home directory with "~", if it fails return it as-is.
	home, err := os.UserHomeDir()
	if err != nil {
		return i.path
	}
	if after, ok := strings.CutPrefix(i.path, home); ok {
		return filepath.Join("~", after)
	}
	return i.path
}

// loadOutdirInfo attempts to load all metrics found in the outdir.
func loadOutdirInfo(workspaceRoot, outDir, manifestPath string) (*outdirInfo, error) {
	start := time.Now()
	fmt.Fprintf(os.Stderr, "load data at %s...", outDir)
	defer func() {
		fmt.Fprintf(os.Stderr, " returned in %v\n", time.Since(start))
	}()

	// Get path relative to workspaceRoot.
	execRel, err := filepath.Rel(workspaceRoot, outDir)
	if err != nil {
		return nil, fmt.Errorf("couldn't get ninja dir relative to workspace: %w", err)
	}

	// Validate manifest path.
	_, err = os.Stat(filepath.Join(outDir, manifestPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &ErrManifestNotExist{outDir, manifestPath}
	}

	// TODO(b/361703735): make sure this works on windows? https://chromium-review.googlesource.com/c/infra/infra/+/5803123/comment/502308d3_ac05bf91/
	outRoot, outSub := filepath.Split(execRel)
	if outRoot != "" {
		if strings.Contains(outSub, "/") {
			return nil, fmt.Errorf("outdir must match pattern `workspace/outroot/outsub`, others are not supported yet")
		}
	}

	outdirInfo := &outdirInfo{
		path:         outDir,
		pathRel:      execRel,
		manifestPath: manifestPath,
		mu:           sync.Mutex{},
	}

	// Attempt to load latest metrics first.
	// Only silently ignore if it doesn't exist, otherwise always return error.
	// TODO(b/349287453): consider tolerate fail, so frontend can show error?
	latestMetricsPath := filepath.Join(outDir, "siso_metrics.json")
	_, err = os.Stat(latestMetricsPath)
	if err == nil {
		latestMetrics, err := loadBuildMetrics(latestMetricsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load latest metrics: %w", err)
		}
		outdirInfo.metrics = append(outdirInfo.metrics, latestMetrics)
		outdirInfo.latestRevID = latestMetrics.ID()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to stat latest metrics: %w", err)
	}

	// Then load revisions if available.
	revPaths, err := filepath.Glob(filepath.Join(outDir, "siso_metrics.*.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to glob revs: %w", err)
	}
	for _, revPath := range revPaths {
		baseName := filepath.Base(revPath)
		matches := sisoMetricsRe.FindStringSubmatch(baseName)
		if matches == nil {
			fmt.Fprintf(os.Stderr, "ignoring invalid %s\n", revPath)
			continue
		}
		revMetrics, err := loadBuildMetrics(revPath)
		if err != nil {
			// TODO(b/349287453): show error in frontend as well?
			fmt.Fprintf(os.Stderr, "ignoring invalid rev metrics %s: %v\n", revPath, err)
			continue
		}
		outdirInfo.metrics = append(outdirInfo.metrics, revMetrics)
	}
	return outdirInfo, nil
}
