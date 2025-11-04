// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"crypto/sha256"
	"io"
)

// stepManifest is a manifest of a step.
// It holds, step's command line, inputs and output of the step,
// and used to check step's re-validation with their hashes.
// TODO: unify cmd hash and edge hash into one hash.
type stepManifest struct {
	// hash of cmdline and rspfileContent.
	cmdHash []byte

	// inputs of the step.
	inputs []string
	// outputs of the step.
	outputs []string
	// hash of inputs/outputs.
	edgeHash []byte
}

func newStepManifest(ctx context.Context, stepDef StepDef) *stepManifest {
	inputs := stepDef.TriggerInputs(ctx)
	outputs := stepDef.Outputs(ctx)
	return &stepManifest{
		cmdHash:  stepDef.CmdHash(),
		inputs:   inputs,
		outputs:  outputs,
		edgeHash: calculateEdgeHash(inputs, outputs),
	}
}

const unitSeparator = "\x1f"

func calculateEdgeHash(inputs, outputs []string) []byte {
	h := sha256.New()
	for _, fname := range inputs {
		io.WriteString(h, fname)
		io.WriteString(h, unitSeparator)
	}
	io.WriteString(h, unitSeparator)
	for _, fname := range outputs {
		io.WriteString(h, fname)
		io.WriteString(h, unitSeparator)
	}
	return h.Sum(nil)
}
