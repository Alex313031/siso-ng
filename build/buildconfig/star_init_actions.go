// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"go.chromium.org/build/siso/build/metadata"
)

// starInitActions returns actions, which contains:
//
//	metadata(key, value): sets key=value in metadata by Starlark config.
func starInitActions(metadata metadata.Metadata) starlark.Value {
	receiver := starMDReceiver{Metadata: metadata}
	return starlarkstruct.FromStringDict(starlark.String("actions"), map[string]starlark.Value{
		"metadata": starlark.NewBuiltin("metadata", starActionsMetadata).BindReceiver(receiver),
	})
}
