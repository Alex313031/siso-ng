// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"errors"
	"fmt"

	"go.starlark.net/starlark"

	"go.chromium.org/build/siso/build/metadata"
)

type starMDReceiver struct {
	Metadata metadata.Metadata
}

func (r starMDReceiver) String() string {
	return "metadata"
}

func (starMDReceiver) Type() string          { return "metadata" }
func (starMDReceiver) Freeze()               {}
func (starMDReceiver) Truth() starlark.Bool  { return starlark.True }
func (starMDReceiver) Hash() (uint32, error) { return 0, errors.New("metadata is not hashable") }

// Starlark value to access metadata.
func starMetadata(metadata metadata.Metadata) starlark.Value {
	dict := starlark.NewDict(metadata.Size())
	// Starlark dictionaries preserve insertion order, so we iterate over the
	// sorted keys to ensure a deterministic order.
	for _, k := range metadata.SortedKeys() {
		dict.SetKey(starlark.String(k), starlark.String(metadata.Get(k)))
	}
	return dict
}

// Starlark value to access flags.
func starFlags(flags map[string]string) starlark.Value {
	dict := starlark.NewDict(len(flags))
	for k, v := range flags {
		dict.SetKey(starlark.String(k), starlark.String(v))
	}
	return dict
}

// Starlark function `metadata(key, value)` to set key=value in metadata.
func starActionsMetadata(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	c, ok := fn.Receiver().(starMDReceiver)
	if !ok {
		return starlark.None, fmt.Errorf("unexpected receiver: %v", fn.Receiver())
	}
	var key, value string
	err := starlark.UnpackArgs("metadata", args, kwargs, "key", &key, "value", &value)
	if err != nil {
		return starlark.None, err
	}
	c.Metadata.Set(key, value)
	return starlark.None, nil
}
