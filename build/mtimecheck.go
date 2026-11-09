// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
)

type targetState struct {
	dirtyErr error
	mtime    time.Time
	changed  bool
}

// targetMap records per-target state during a build, normalizing the key
// (DirTargetPath) so a directory artifact stored as "gen/" and looked up as
// "gen" (or vice versa) always meet under the same key.
type targetMap struct {
	m sync.Map
}

func (t *targetMap) Store(target string, st targetState) {
	t.m.Store(DirTargetPath(target), st)
}

func (t *targetMap) Load(target string) (targetState, bool) {
	v, ok := t.m.Load(DirTargetPath(target))
	if !ok {
		return targetState{}, false
	}
	return v.(targetState), true
}

// needToRun checks whether step needs to run or not.
func (b *Builder) needToRun(ctx context.Context, stepDef StepDef, stepManifest *stepManifest) bool {
	if stepDef.IsPhony() {
		var dirtyErr error
		_, mtime, err := inputMtime(ctx, b, stepDef)

		if errors.Is(err, errDirty) {
			// if phony's inputs are dirty, mark this phony's output as dirty.
			dirtyErr = err
		}
		for _, outpath := range stepManifest.outputs {
			b.targets.Store(string(outpath), targetState{
				dirtyErr: dirtyErr,
				mtime:    mtime,
			})
			if dirtyErr != nil || log.V(1) {
				clog.Infof(ctx, "phony output %s dirty=%v mtime=%v", outpath, dirtyErr, mtime)
			}
		}
		// nothing to run for phony target.
		return false
	}
	return !b.checkUpToDate(ctx, stepDef, stepManifest)
}

// checkUpToDate returns true if outputs are already up-to-date and
// no need to run command.
func (b *Builder) checkUpToDate(ctx context.Context, stepDef StepDef, stepManifest *stepManifest) bool {
	ctx, span := trace.NewSpan(ctx, "mtime-check")
	defer span.Close(nil)

	generator := stepDef.Binding("generator") != ""

	out0, outmtime, cmdhash, edgehash := outputMtime(ctx, b, stepManifest.outputs, stepDef.Binding("restat") != "")
	lastIn, inmtime, err := inputMtime(ctx, b, stepDef)

	// TODO(b/288419130): make sure it covers all cases as ninja does.

	outname := b.path.MaybeToRelative(ctx, string(out0))
	lastInName := b.path.MaybeToRelative(ctx, lastIn)
	if err != nil {
		clog.Infof(ctx, "need %v", err)
		reason := "missing-inputs"
		switch {
		case errors.Is(err, ErrMissingDeps):
			reason = "missing-deps"
		case errors.Is(err, ErrStaleDeps):
			reason = "stale-deps"
		case errors.Is(err, errDirty):
			reason = "dirty"
		}
		span.SetAttr("run-reason", reason)
		fmt.Fprintf(b.explainWriter, "deps for %s %s: %v\n", outname, reason, err)
		return false
	}
	if outmtime.IsZero() {
		clog.Infof(ctx, "need: output doesn't exist")
		span.SetAttr("run-reason", "no-output")
		fmt.Fprintf(b.explainWriter, "output %s doesn't exist\n", outname)
		return false
	}
	if inmtime.After(outmtime) {
		clog.Infof(ctx, "need: in:%s > out:%s %s: in:%s out:%s", lastIn, out0, inmtime.Sub(outmtime), inmtime, outmtime)
		span.SetAttr("run-reason", "dirty")
		fmt.Fprintf(b.explainWriter, "output %s older than most recent input %s: out:%s in:+%s\n", outname, lastInName, outmtime.Format(time.RFC3339Nano), inmtime.Sub(outmtime))
		return false
	}
	if !generator && !bytes.Equal(cmdhash, stepManifest.cmdHash) {
		clog.Infof(ctx, "need: cmdhash differ %q -> %q", base64.StdEncoding.EncodeToString(cmdhash), base64.StdEncoding.EncodeToString(stepManifest.cmdHash))
		span.SetAttr("run-reason", "cmdhash-update")
		if len(cmdhash) == 0 {
			fmt.Fprintf(b.explainWriter, "command line not found in log for %s\n", outname)
		} else {
			fmt.Fprintf(b.explainWriter, "command line changed for %s\n", outname)
		}
		return false
	}
	if len(edgehash) == 0 {
		// old version of siso didn't record edgehash...
		// TODO: remove this condition?
		clog.Warningf(ctx, "missing edgehash in output")
	} else if !bytes.Equal(edgehash, stepManifest.edgeHash) {
		reasonLog := fmt.Sprintf("edgehash differ %q -> %q", base64.StdEncoding.EncodeToString(edgehash), base64.StdEncoding.EncodeToString(stepManifest.edgeHash))
		reasonExplain := "edge changed"
		if sandbox := stepDef.Sandbox(); len(sandbox) > 0 {
			// we might just toggle sandboxing on?
			edgehashNoSandbox := calculateEdgeHash(stepManifest.inputs, stepManifest.outputs, nil)
			if bytes.Equal(edgehash, edgehashNoSandbox) {
				reasonLog = "toggle sandbox on: " + reasonLog
				reasonExplain = "toggle sandbox on"
			}
		} else {
			// we might just toggle sandboxing off?
			edgehashWithSandbox := calculateEdgeHash(stepManifest.inputs, stepManifest.outputs, []string{"sandbox"})
			if bytes.Equal(edgehash, edgehashWithSandbox) {
				reasonLog = "toggle sandbox off: " + reasonLog
				reasonExplain = "toggle sandbox off"
			}
		}
		clog.Infof(ctx, "need: %s", reasonLog)
		fmt.Fprintf(b.explainWriter, "%s for %s\n", reasonExplain, outname)
		return false
	}
	if b.clobber {
		clog.Infof(ctx, "need: clobber")
		span.SetAttr("run-reason", "clobber")
		// explain once at the beginning of the build.
		return false
	}
	numOuts := len(stepManifest.outputs)
	depFile := stepDef.Depfile(ctx)
	if depFile != "" {
		numOuts++
	}
	localOutputs := make([]path.Path, 0, numOuts)
	seen := make(map[path.Path]bool)
	for _, outPath := range stepManifest.outputs {
		if seen[outPath] {
			continue
		}
		seen[outPath] = true
		if !b.hashFS.NeedFlush(ctx, b.path.WorkspaceRoot, outPath) {
			continue
		}
		localOutputs = append(localOutputs, outPath)
	}
	if depFile != "" {
		switch stepDef.Binding("deps") {
		case "gcc", "msvc":
		default:
			if b.hashFS.NeedFlush(ctx, b.path.WorkspaceRoot, depFile) {
				localOutputs = append(localOutputs, depFile)
			}
		}
	}
	if len(localOutputs) > 0 {
		err := b.hashFS.Flush(ctx, b.path.WorkspaceRoot, localOutputs)
		if err != nil {
			clog.Infof(ctx, "need: no local outputs %q: %v", localOutputs, err)
			span.SetAttr("run-reason", "missing-local-outputs")
			fmt.Fprintf(b.explainWriter, "output %s flush error %s: %v\n", outname, localOutputs, err)
			return false
		}
		if log.V(1) {
			clog.Infof(ctx, "flush all outputs %s", localOutputs)
		}
	}

	if log.V(1) {
		clog.Infof(ctx, "skip: in:%s < out:%s %s", lastIn, out0, outmtime.Sub(inmtime))
	}
	span.SetAttr("skip", true)
	return true
}

// outputMtime returns the oldest modified output, its timestamp and
// command hash / edge hash that produced the outputs of the step.
func outputMtime(ctx context.Context, b *Builder, outputs []path.Path, restat bool) (path.Path, time.Time, []byte, []byte) {
	var oerr error
	var outmtime time.Time
	var outcmdhash []byte
	var edgehash []byte
	var out0 path.Path
	for i, outPath := range outputs {
		statPath := DirTargetPath(string(outPath))
		fi, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, path.New(statPath))
		if err != nil {
			if oerr == nil {
				out0 = outPath
				oerr = err
			}
			continue
		}
		if log.V(1) {
			clog.Infof(ctx, "out-cmdhash %d:%s %s", i, outPath, base64.StdEncoding.EncodeToString(fi.CmdHash()))
		}
		if i == 0 {
			outcmdhash = fi.CmdHash()
			edgehash = fi.EdgeHash()
		}
		if !bytes.Equal(outcmdhash, fi.CmdHash()) {
			if log.V(1) {
				clog.Infof(ctx, "out-cmdhash differ %s %s->%s", outPath, base64.StdEncoding.EncodeToString(outcmdhash), base64.StdEncoding.EncodeToString(fi.CmdHash()))
			}
			outcmdhash = nil
		}
		var t time.Time
		if restat {
			t = fi.UpdatedTime()
		} else {
			t = fi.ModTime()
		}
		t, walkErr := effectiveMtime(b.hashFS.FileSystem(ctx, b.path.WorkspaceRoot), statPath, t, IsDirTarget(string(outPath)))
		if walkErr != nil {
			// Can't determine the output's mtime: signal not-up-to-date.
			if oerr == nil {
				out0 = outPath
				oerr = walkErr
			}
			clog.Warningf(ctx, "walk dir output %s: %v", outPath, walkErr)
			continue
		}
		if outmtime.IsZero() {
			outmtime = t
			out0 = outPath
			continue
		}
		if outmtime.After(t) {
			outmtime = t
			out0 = outPath
		}
	}
	if oerr != nil {
		outmtime = time.Time{}
	}
	return out0, outmtime, outcmdhash, edgehash
}

var errDirty = errors.New("dirty")

// inputMtime returns the last modified input and its modified timestamp.
// it will return errDirty if input file has been updated in the build session.
func inputMtime(ctx context.Context, b *Builder, stepDef StepDef) (string, time.Time, error) {
	var inmtime time.Time
	lastIn := ""
	ins := stepDef.TriggerInputs(ctx)
	depsIter, err := stepDef.DepInputs(ctx)
	if err != nil {
		return "", inmtime, fmt.Errorf("failed to load deps: %w", err)
	}
	var retErr error
	appendSeq(ins, depsIter)(func(in path.Path) bool {
		var mtime time.Time
		var changed bool
		isDir := IsDirTarget(string(in))
		lookupPath := DirTargetPath(string(in))
		ts, ok := b.targets.Load(string(in))
		if ok {
			// seen/phony target
			if ts.dirtyErr != nil {
				retErr = fmt.Errorf("input %s: %w", in, ts.dirtyErr)
				return false
			}
			mtime = ts.mtime
			changed = ts.changed
		} else {
			fsys := b.hashFS.FileSystem(ctx, b.path.WorkspaceRoot)
			fi, err := fsys.StatMtime(lookupPath)
			if log.V(1) {
				clog.Infof(ctx, "input %q -> %v", in, err)
			}
			for _, fi := range fsys.Visited(fi) {
				if mtime.Before(fi.ModTime()) {
					mtime = fi.ModTime()
				}
				if !changed {
					changed = fi.IsChanged()
				}
				if len(fi.CmdHash()) > 0 {
					// generated file.
					// we don't resolve symlink.
					// android platform uses dangling
					// symlink for output, so ignore
					// Stat error.
					err = nil
					break
				}
			}
			if err != nil {
				b.targets.Store(lookupPath, targetState{
					dirtyErr: err,
				})
				retErr = fmt.Errorf("input %s: %w", in, err)
				return false
			}
			emtime, walkErr := effectiveMtime(fsys, lookupPath, mtime, isDir)
			if walkErr != nil {
				// Can't determine the input's state: treat as a rebuild reason.
				b.targets.Store(lookupPath, targetState{
					dirtyErr: walkErr,
				})
				retErr = fmt.Errorf("input %s: %w", in, walkErr)
				return false
			}
			mtime = emtime
			b.targets.Store(lookupPath, targetState{
				mtime:   mtime,
				changed: changed,
			})
		}
		if inmtime.Before(mtime) {
			inmtime = mtime
			lastIn = string(in)
		}
		if changed {
			retErr = fmt.Errorf("input %s: %w", in, errDirty)
			return false
		}
		return true
	})
	if retErr != nil {
		return "", inmtime, retErr
	}
	return lastIn, inmtime, nil
}

// effectiveMtime returns base for a file, or the newest mtime in the tree for
// a directory artifact (a directory's own mtime ignores deep changes). On a walk
// error it returns base and the error.
func effectiveMtime(fsys fs.FS, statPath string, base time.Time, isDir bool) (time.Time, error) {
	if !isDir {
		return base, nil
	}
	eff, err := dirEffectiveMtime(fsys, statPath)
	if err != nil {
		return base, err
	}
	if eff.After(base) {
		return eff, nil
	}
	return base, nil
}

// dirEffectiveMtime walks a directory recursively and returns the maximum mtime across all files and subdirectories.
func dirEffectiveMtime(fsys fs.FS, dir string) (time.Time, error) {
	var maxMtime time.Time
	err := fs.WalkDir(fsys, dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(maxMtime) {
			maxMtime = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("walk dir %s: %w", dir, err)
	}
	return maxMtime, nil
}

func appendSeq(ins []path.Path, iter func(func(path.Path) bool)) func(yield func(path.Path) bool) {
	// ins should be unique inputs.
	// iter (deps log) should also be unique inputs,
	// but input in iter may be duplicated with input in ins.
	seen := make(map[path.Path]bool, len(ins))
	return func(yield func(path.Path) bool) {
		for _, in := range ins {
			seen[in] = true
			if !yield(in) {
				return
			}
		}
		iter(func(in path.Path) bool {
			if seen[in] {
				return true
			}
			return yield(in)
		})
	}
}
