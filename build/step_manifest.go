// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"crypto/sha256"
	"io"
	"maps"
	"slices"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/path"
)

// stepManifest is a manifest of a step.
// It holds, step's command line, inputs and output of the step,
// and used to check step's re-validation with their hashes.
// TODO: unify cmd hash and edge hash into one hash.
type stepManifest struct {
	// hash of cmdline and rspfileContent (and normalized env for local steps).
	cmdHash []byte

	// inputs of the step.
	inputs []path.Path
	// outputs of the step.
	outputs []path.Path
	// hash of inputs/outputs/extra
	// extra: sandbox
	edgeHash []byte
}

func (b *Builder) newStepManifest(ctx context.Context, stepDef StepDef) *stepManifest {
	inputs := stepDef.TriggerInputs(ctx)
	outputs := stepDef.Outputs(ctx)
	var extra []string
	if sandbox := stepDef.Sandbox(); len(sandbox) > 0 {
		extra = append(extra, "sandbox")
	}
	cmdHash := stepDef.CmdHash()
	if len(b.envHash) > 0 && !stepDef.IsPhony() {
		stepDef.EnsureRule(ctx)
		isRemote := b.remoteExec != nil && stepDef.Pure() && len(stepDef.Platform()) > 0 && stepDef.Binding("pool") != "console"
		if !isRemote {
			h := sha256.New()
			h.Write(cmdHash)
			h.Write(b.envHash)
			cmdHash = h.Sum(nil)
		}
	}
	return &stepManifest{
		cmdHash:  cmdHash,
		inputs:   inputs,
		outputs:  outputs,
		edgeHash: calculateEdgeHash(inputs, outputs, extra),
	}
}

const unitSeparator = "\x1f"

func calculateEnvHash(env []string, substitutions map[string]string, trimPrefixes []string, omits map[string]bool) []byte {
	if len(env) == 0 {
		return nil
	}
	envMap := execute.NormalizeEnvVars(env, substitutions, trimPrefixes, func(key string) bool {
		return !omits[key]
	})
	if len(envMap) == 0 {
		return nil
	}
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(envMap)) {
		io.WriteString(h, k)
		io.WriteString(h, "=")
		io.WriteString(h, envMap[k])
		io.WriteString(h, unitSeparator)
	}
	return h.Sum(nil)
}

func calculateEdgeHash(inputs, outputs []path.Path, extra []string) []byte {
	h := sha256.New()
	for _, fname := range inputs {
		io.WriteString(h, string(fname))
		io.WriteString(h, unitSeparator)
	}
	io.WriteString(h, unitSeparator)
	for _, fname := range outputs {
		io.WriteString(h, string(fname))
		io.WriteString(h, unitSeparator)
	}
	if len(extra) > 0 {
		io.WriteString(h, unitSeparator)
		for _, name := range extra {
			io.WriteString(h, name)
			io.WriteString(h, unitSeparator)
		}
	}
	return h.Sum(nil)
}
