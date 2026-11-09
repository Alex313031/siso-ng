// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"time"

	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

type twoPhaseCaching interface {
	// ComputeLookupKey computes two phase cache lookup key for the step.
	ComputeLookupKey(ctx context.Context, step *Step) (string, error)

	// Check checks two phase caching for the step using lookupKey.
	// return nil when cache hit and populated outputs.
	// return non-nil error otherwise.
	Check(ctx context.Context, lookupKey string, step *Step) error

	// Add adds two phase caching for the step with lookupKey.
	Add(ctx context.Context, lookupKey string, step *Step) error
}

func (b *Builder) twoPhaseCachingLookup(ctx context.Context, step *Step) (bool, error) {
	ctx, span := trace.NewSpan(ctx, "twophasecaching-lookup")
	defer span.Close(nil)
	step.setPhase(stepCacheCheck)
	// disable canonicalize dir to match input root with local
	// TODO: canonicalize dir in matchInputRoot.
	step.cmd.CanonicalizeDir = false

	err := b.twoPhaseCachingSema.Do(ctx, func(ctx context.Context) error {
		lookupKey, err := b.twoPhaseCaching.ComputeLookupKey(ctx, step)
		if err != nil {
			return err
		}
		step.metrics.TwoPhaseCachingKey = lookupKey
		return b.twoPhaseCaching.Check(ctx, lookupKey, step)
	})
	if err == nil {
		return true, nil
	}
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unimplemented:
		return true, fmt.Errorf("two phase cache unsupported: %w", err)
	}
	clog.Warningf(ctx, "two phase cache: %v", err)
	return false, err
}

type actionCacheMap interface {
	Add(context.Context, string, digest.Digest) error
	List(context.Context, string) iter.Seq2[*rpb.Action, error]
}

type reapiTwoPhaseCaching struct {
	b              *Builder
	actionCacheMap actionCacheMap
}

func (reapiTwoPhaseCaching) ComputeLookupKey(ctx context.Context, step *Step) (string, error) {
	pcmd := step.cmd.Clone()
	if experiments.Enabled("expand-phony-trigger-inputs", "") {
		pcmd.Inputs = step.def.ExpandedTriggerInputs(ctx)
	} else {
		pcmd.Inputs = step.def.TriggerInputs(ctx)
	}
	pcmd.Pure = true // not pure, but to make calculate digest.
	d, err := pcmd.Digest(ctx, nil)
	if err != nil {
		return "", err
	}
	return d.String(), nil
}

func (rt reapiTwoPhaseCaching) Check(ctx context.Context, lookupKey string, step *Step) error {
	if log.V(1) {
		clog.Infof(ctx, "two phase cache: lookup=%s", lookupKey)
	}
	ocmd := step.cmd
	started := time.Now()
	step.metrics.CacheStartTime = IntervalMetric(started.Sub(rt.b.start))
	defer func() {
		step.metrics.CacheTime = IntervalMetric(time.Since(started))
	}()

	commandDigest, err := step.cmd.CommandDigest(ctx, nil)
	if err != nil {
		clog.Warningf(ctx, "failed to compute command digest: %v", err)
	}
	// RBE returns Order: newest / most recently added first.
	// The 2PC proposal (go/2-phase-caching-api-proposal) noted from ABFS
	// metrics that matching candidates are almost always found within
	// the first 10 candidates
	// TODO: adapt number of candidate by action
	// i.e. cheap action should be small, but heavy action could be large.
	const maxCandidates = 10
	nactions := 0
	for action, err := range rt.actionCacheMap.List(ctx, lookupKey) {
		nactions++
		if err != nil {
			step.cmd = ocmd
			return fmt.Errorf("list actions: %w", err)
		}
		if nactions > maxCandidates {
			clog.Warningf(ctx, "too many candidates in %q: %d", lookupKey, nactions)
			return fmt.Errorf("cache not found. too many candidates %d for %s", nactions, lookupKey)
		}
		inputs, outputs, err := rt.matchAction(ctx, step, action, commandDigest)
		if err != nil {
			clog.Infof(ctx, "mismatch action %s: %v", action, err)
			continue
		}
		step.cmd = ocmd.Clone()
		actionMsg, err := blob.FromProtoMessage(rt.b.hashFS.DigestFunction(), action)
		if log.V(1) {
			clog.Infof(ctx, "match action %s => %s: %v", action, actionMsg, err)
		}
		if err == nil {
			step.cmd.SetActionDigest(actionMsg.Digest())
		}
		// use the same inputs, outputs as action.
		step.cmd.Inputs = path.Paths(inputs)
		// if outputs returned matchAction is empty, it means
		// it matches by digest, so no need to modify step.cmd.Outputs
		if len(outputs) > 0 {
			step.cmd.Outputs = path.Paths(outputs)
			if step.cmd.Depfile != "" {
				// but need to delete depfile from outputs.
				// depfile is added in AllOutputs.
				step.cmd.Outputs = slices.DeleteFunc(step.cmd.Outputs, func(s path.Path) bool {
					return s == step.cmd.Depfile
				})
			}
			step.cmd.InitOutputs()
		}
		if log.V(2) {
			clog.Infof(ctx, "inputs %q", step.cmd.Inputs)
			clog.Infof(ctx, "outputs %q", step.cmd.Outputs)
		}
		step.cmd.Pure = true
		err = rt.b.execRemoteCache(ctx, step)
		if err != nil {
			// cache miss
			clog.Warningf(ctx, "cache miss: %v", err)
			step.cmd = ocmd
			continue
		}
		// cache hit
		if log.V(1) {
			clog.Infof(ctx, "cache hit in two phase caching")
		}
		step.metrics.TwoPhaseCachingActions = nactions
		step.metrics.TwoPhaseCacheHit = true
		return nil
	}
	step.cmd = ocmd
	step.metrics.TwoPhaseCachingActions = nactions
	return fmt.Errorf("cache miss %d for %s", nactions, lookupKey)
}

// matchAction checks whether action matches step.
func (rt reapiTwoPhaseCaching) matchAction(ctx context.Context, step *Step, action *rpb.Action, localDigest digest.Digest) (inputs, outputs []string, retErr error) {
	ctx, span := trace.NewSpan(ctx, "twophasecaching-match-action")
	defer span.Close(nil)
	cmdDigest := digest.FromProto(action.GetCommandDigest())
	if cmdDigest != localDigest {
		cmd := &rpb.Command{}
		err := rt.b.reapiclient.Proto(ctx, cmdDigest, cmd)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to fetch command for %s: %w", cmdDigest, err)
		}
		// check command line
		if !slices.Equal(step.cmd.Args, cmd.GetArguments()) {
			return nil, nil, fmt.Errorf("arguments mismatch with %s", cmdDigest)
		}
		if !step.cmd.WorkDir.Equal(path.New(cmd.GetWorkingDirectory())) {
			return nil, nil, fmt.Errorf("working_dir mismatch %q != %q", step.cmd.WorkDir, cmd.GetWorkingDirectory())
		}
		if !slices.EqualFunc(step.cmd.EnvVars(), cmd.GetEnvironmentVariables(), func(a, b *rpb.Command_EnvironmentVariable) bool {
			return a.GetName() == b.GetName() && a.GetValue() == b.GetValue()
		}) {
			return nil, nil, fmt.Errorf("environment_variables mismatch with %s", cmdDigest)
		}

		for _, output := range cmd.GetOutputFiles() { //nolint:staticcheck // existing deprecation
			outputs = append(outputs, rt.b.path.MaybeFromRelative(ctx, output))
		}
		for _, output := range cmd.GetOutputDirectories() { //nolint:staticcheck // existing deprecation
			outputs = append(outputs, rt.b.path.MaybeFromRelative(ctx, output))
		}
		for _, output := range cmd.GetOutputPaths() {
			outputs = append(outputs, rt.b.path.MaybeFromRelative(ctx, output))
		}
	}

	// check input root
	inputRootDigest := digest.FromProto(action.GetInputRootDigest())
	inputs, err := rt.matchInputRoot(ctx, inputRootDigest)
	if err != nil {
		return nil, nil, fmt.Errorf("input_root mismatch with %s: %w", inputRootDigest, err)
	}
	return inputs, outputs, nil
}

func (rt reapiTwoPhaseCaching) matchInputRoot(ctx context.Context, inputRootDigest digest.Digest) ([]string, error) {
	ctx, span := trace.NewSpan(ctx, "twophasecaching-match-input-root")
	defer span.Close(nil)
	var inputs, emptyDirs []string
	err := rt.b.reapiclient.WalkDir(ctx, inputRootDigest, func(dname string, dd digest.Digest, dir *rpb.Directory) error {
		if log.V(2) {
			clog.Infof(ctx, "walkdir dir %q: %v", dname, dir)
		}
		if len(dir.Files) == 0 && len(dir.Directories) == 0 && len(dir.Symlinks) == 0 {
			// An empty directory is an input by itself. WalkDir visits
			// a directory only after its parent's callback succeeded,
			// and that callback verified that it exists locally as a
			// directory. The root has no parent and is not an input.
			if dname != "" {
				emptyDirs = append(emptyDirs, dname)
			}
			return nil
		}
		spec := rt.b.dirSpec(dname, dd, dir)
		if spec.children != nil && rt.b.hashFS.MatchDir(ctx, spec.fulldir, spec.children) {
			// Same result as the check below: every file and
			// directory is already known to match. The inputs
			// are the files, in order.
			inputs = append(inputs, spec.names[:spec.nfiles]...)
			return nil
		}
		names := spec.names
		m := make(map[string]merkletree.Entry, len(names))
		i := 0
		for _, file := range dir.Files {
			m[names[i]] = merkletree.Entry{
				Name:         path.Path(names[i]),
				Data:         blob.NewData(nil, digest.FromProto(file.Digest)),
				IsExecutable: file.IsExecutable,
			}
			i++
		}
		for range dir.Directories {
			m[names[i]] = merkletree.Entry{
				Name: path.Path(names[i]),
			}
			i++
		}
		for _, symlink := range dir.Symlinks {
			m[names[i]] = merkletree.Entry{
				Name:   path.Path(names[i]),
				Target: symlink.Target,
			}
			i++
		}
		if log.V(2) {
			clog.Infof(ctx, "walkdir check %q", names)
		}
		// entries from workspace root to get symlink correctly.
		ents, err := rt.b.hashFS.Entries(ctx, rt.b.path.WorkspaceRoot, path.Paths(names))
		if err != nil {
			return fmt.Errorf("entries: %w", err)
		}
		if log.V(2) {
			clog.Infof(ctx, "walkdir entries %v", len(ents))
		}
		if len(ents) != len(names) {
			return fmt.Errorf("missing some entries locally in %q (local:%d, expected:%d)", dname, len(ents), len(names))
		}
		for _, ent := range ents {
			e, ok := m[string(ent.Name)]
			if !ok {
				return fmt.Errorf("missing %q in %q", ent.Name, dname)
			}
			if !e.Data.IsZero() {
				// want file
				if ent.Data.Digest() != e.Data.Digest() {
					return fmt.Errorf("mismatch %q in %q: digest local:%s != want:%s", ent.Name, dname, ent.Data.Digest(), e.Data.Digest())
				}
				if ent.IsExecutable != e.IsExecutable {
					return fmt.Errorf("mismatch %q in %q: is_executable local:%t != want:%t", ent.Name, dname, ent.IsExecutable, e.IsExecutable)
				}
				inputs = append(inputs, string(e.Name))
				continue
			}
			if e.Target != "" {
				// want symlink
				if ent.Target != e.Target {
					return fmt.Errorf("mismatch %q in %q: target local:%q != want:%q", ent.Name, dname, ent.Target, e.Target)
				}
				inputs = append(inputs, string(e.Name))
				continue
			}
			// want dir
			if !ent.Data.IsZero() {
				return fmt.Errorf("mismatch %q in %q: local digest:%s want:dir", ent.Name, dname, ent.Data.Digest())
			}
			if ent.Target != "" {
				fsys := rt.b.hashFS.FileSystem(ctx, rt.b.path.WorkspaceRoot)
				fi, err := fsys.StatIfExists(string(ent.Name))
				if err != nil {
					return fmt.Errorf("mismatch %q in %q: bad symlink %q: %v", ent.Name, dname, ent.Target, err)
				}
				if !fi.IsDir() {
					return fmt.Errorf("mismatch %q in %q: local symlink:%q want:dir", ent.Name, dname, ent.Target)
				}
				// match if this is symlink to directory.
			}
		}
		if log.V(2) {
			clog.Infof(ctx, "walkdir dir %q: match", dname)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(emptyDirs)
	inputs = append(inputs, slices.Compact(emptyDirs)...)
	return inputs, nil
}

func (rt reapiTwoPhaseCaching) Add(ctx context.Context, lookupKey string, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "twophasecaching-add-action-in-cache")
	defer span.Close(nil)
	// associate action to lookupKey.
	clog.Infof(ctx, "two phase cache: add lookup=%s action=%s", lookupKey, step.cmd.ActionDigest())
	return rt.actionCacheMap.Add(ctx, lookupKey, step.cmd.ActionDigest())
}
