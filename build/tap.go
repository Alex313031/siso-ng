// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/localexec"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/path"
)

var errNoTapData = errors.New("no tap data")

func (b *Builder) tapCanonicalizeCmd(ctx context.Context, cmd *execute.Cmd) error {
	res, _ := cmd.ActionResult()
	tapData, tapped := localexec.ExtractTapResult(res)
	if !tapped {
		return errNoTapData
	}
	if tapData.Error != "" {
		return fmt.Errorf("tap error: %v", tapData.Error)
	}

	var nInputs, nOutputs int
	var nInputsDiscarded, nOutputsDiscarded int
	var nOutputsInDir int
	// ignore out of workspace root
	// TODO: use with input root absolute path?
	// TODO: just use detected inputs?
	seen := make(map[path.Path]bool)
	for _, input := range cmd.AllInputs() {
		seen[input] = true
		nInputs++
	}
	// os.TempDir() is $TMPDIR or /tmp.
	ignores := map[path.Path]struct{}{path.New(os.TempDir()): {}}
	ignored := 0
	if len(cmd.Env) > 0 {
		for _, env := range cmd.Env {
			tmpdir, ok := strings.CutPrefix(env, "TMPDIR=")
			if ok {
				if !filepath.IsAbs(tmpdir) {
					clog.Warningf(ctx, "ignore TMPDIR: not absolute path: %q", tmpdir)
					continue
				}
				ignores[path.New(tmpdir)] = struct{}{}
			}
		}
	}
	shouldIgnore := func(op string, p path.Path) bool {
		for ignore := range ignores {
			if p.HasPrefix(ignore) {
				if log.V(1) {
					clog.Infof(ctx, "ignore %s %q in %q", op, p, ignore)
				}
				return true
			}
		}
		return false
	}
	underOutputDir := func(p path.Path) bool {
		for _, dir := range cmd.OutputDirs {
			if p.HasPrefix(dir) {
				if log.V(1) {
					clog.Infof(ctx, "in dir %q under %q", p, dir)
				}
				return true
			}
		}
		for _, dir := range cmd.AuxiliaryLogOutputDirs {
			if p.HasPrefix(dir) {
				if log.V(1) {
					clog.Infof(ctx, "in dir %q under %q", p, dir)
				}
				return true
			}
		}
		return false
	}
	var tapDetected int

	fsys := b.hashFS.FileSystem(ctx, b.path.WorkspaceRoot)

	for _, input := range tapData.Reads {
		if shouldIgnore("reads", path.New(input)) {
			ignored++
			continue
		}
		rel, err := filepath.Rel(b.path.WorkspaceRoot, input)
		if err != nil {
			clog.Warningf(ctx, "reads relpath %q: %v", input, err)
			nInputsDiscarded++
			continue
		}
		if !filepath.IsLocal(rel) {
			nInputsDiscarded++
			continue
		}
		relPath := path.New(rel)
		fi, err := fsys.Stat(string(relPath))
		if errors.Is(err, fs.ErrNotExist) {
			nInputsDiscarded++
			continue
		}
		tapDetected++
		if seen[relPath] {
			continue
		}
		seen[relPath] = true
		cmd.Inputs = append(cmd.Inputs, relPath)
		for _, p := range fsys.VisitedPaths(fi) {
			pPath := path.New(p)
			if seen[pPath] {
				continue
			}
			seen[pPath] = true
			cmd.Inputs = append(cmd.Inputs, pPath)
		}
	}
	clear(seen)

	// remember declared outputs.
	for _, output := range cmd.AllOutputs() {
		seen[output] = true
		nOutputs++
	}

	// handle deletes first.
	// rbetap and tapr may report unordered sets of writes and deletes,
	// so both unlink-before-write (output recreated), and
	// write-before-unlink (temp file deleted before exit) appear in
	// both writes and deletes.
	// Forget undeclared deletes from HashFS first. the write pass
	// below will re-check local disk to distinguish the two cases.
	for _, del := range tapData.Deletes {
		rel, err := filepath.Rel(b.path.WorkspaceRoot, del)
		if err != nil {
			clog.Warningf(ctx, "deletes relpath %q: %v", del, err)
			continue
		}
		if !filepath.IsLocal(rel) {
			continue
		}
		relPath := path.New(rel)
		if seen[relPath] || underOutputDir(relPath) {
			// declared outputs and output dirs are already updated from local disk by RecordOutputsFromLocal.
			// if Forget, it would lost cmdhash etc.
			continue
		}
		_, err = b.hashFS.Stat(ctx, b.path.WorkspaceRoot, relPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		b.hashFS.Forget(ctx, b.path.WorkspaceRoot, []path.Path{relPath})
		clog.Infof(ctx, "delete %q", relPath)
	}

	// check undeclared but tap discovered outputs.
	// we need to capture them from local disk and update them in hashfs.
	var tapDiscoveredOutputs []path.Path
	for _, output := range tapData.Writes {
		if shouldIgnore("writes", path.New(output)) {
			ignored++
			continue
		}
		rel, err := filepath.Rel(b.path.WorkspaceRoot, output)
		if err != nil {
			clog.Warningf(ctx, "writes relpath %q: %v", output, err)
			nOutputsDiscarded++
			continue
		}
		if !filepath.IsLocal(rel) {
			nOutputsDiscarded++
			continue
		}
		relPath := path.New(rel)
		if seen[relPath] {
			_, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, relPath)
			if err != nil {
				nOutputsDiscarded++
				continue
			}
			tapDetected++
			continue
		}
		seen[relPath] = true
		if underOutputDir(relPath) {
			_, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, relPath)
			if err != nil {
				nOutputsDiscarded++
				continue
			}
			tapDetected++
			// relPath is covered by cmd.OutputDirs or
			// AuxiliaryLogOutputDirs, so no need to
			// add as output files. b/555559994
			nOutputsInDir++
			continue
		}
		tapDiscoveredOutputs = append(tapDiscoveredOutputs, relPath)
	}
	var outputDirs []path.Path
	var nOutputsDiscovered int
	if len(tapDiscoveredOutputs) > 0 {
		// RetrieveUpdateEntriesFromLocal checks local disk for each path
		// - unlink-before-write: file exists on disk -> returned in
		//   tapDiscoveredEnts, updated in HashFS, and added to
		//   cmd.Outputs.
		// - write-before-unlink: file does not exist on disk ->
		//   omitted from tapDiscoveredEnts (counted in
		//   nOutputsDiscarded) and excluded from cmd.Outputs.
		tapDiscoveredEnts := b.hashFS.RetrieveUpdateEntriesFromLocal(ctx, b.path.WorkspaceRoot, tapDiscoveredOutputs)
		nOutputsDiscarded += len(tapDiscoveredOutputs) - len(tapDiscoveredEnts)
		tapDetected += len(tapDiscoveredEnts)
		now := time.Now()
		for i, ent := range tapDiscoveredEnts {
			ent.ModTime = now
			ent.UpdatedTime = now
			ent.IsChanged = true
			ent.IsLocal = true
			// cmdhash is only set for declared outputs
			ent.Action = cmd.ActionDigest()
			tapDiscoveredEnts[i] = ent
			if ent.Mode.IsDir() {
				outputDirs = append(outputDirs, ent.Name)
				continue
			}
			cmd.Outputs = append(cmd.Outputs, ent.Name)
		}
		err := b.hashFS.Update(ctx, b.path.WorkspaceRoot, tapDiscoveredEnts)
		if err != nil {
			return fmt.Errorf("tap update entries %q: %v", tapDiscoveredOutputs, err)
		}
		nOutputsDiscovered = len(tapDiscoveredEnts)
	}

	emptyDirs := slices.DeleteFunc(outputDirs, func(outDir path.Path) bool {
		for _, out := range cmd.Outputs {
			if out.HasPrefix(outDir) {
				return true
			}
		}
		dents, err := b.hashFS.ReadDir(ctx, b.path.WorkspaceRoot, outDir)
		if err != nil || len(dents) > 0 {
			return true
		}
		// empty directory
		return false

	})
	cmd.OutputDirs = append(cmd.OutputDirs, emptyDirs...)
	cmd.InitOutputs()
	clog.Infof(ctx, "tap canonicalized detected=%d inputs=%d (discarded:%d) ->%d outputs=%d (indir:%d discarded:%d discovered:%d emptyDir:%d) ->%d ignored=%d", tapDetected, nInputs, nInputsDiscarded, len(cmd.Inputs), nOutputs, nOutputsInDir, nOutputsDiscarded, nOutputsDiscovered, len(emptyDirs), len(cmd.Outputs), ignored)
	if tapDetected == 0 {
		return fmt.Errorf("tap detected=0: %s", tapData)
	}
	// now tap results are applied to cmd, so we can consider
	// this cmd is pure, thus cacheable.
	cmd.Pure = true

	return nil
}
