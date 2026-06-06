// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"embed"
	"runtime"

	starjson "go.starlark.net/lib/json"
	starmath "go.starlark.net/lib/math"
	starproto "go.starlark.net/lib/proto"
	startime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// embeds these Starlark files for @builtin.
//
//go:embed encoding.star path.star runtime.star struct.star lib/gn.star
var builtinStar embed.FS

func builtinModule() map[string]starlark.Value {
	runtimeModule := &starlarkstruct.Module{
		Name: "runtime",
		Members: map[string]starlark.Value{
			"num_cpu": starlark.MakeInt(runtime.GOMAXPROCS(0)),
			"os":      starlark.String(runtime.GOOS),
			"arch":    starlark.String(runtime.GOARCH),
			// need to include os version (to select platform container images)?
		},
	}
	runtimeModule.Freeze()

	return map[string]starlark.Value{
		"__builtin_runtime": runtimeModule,
		"__builtin_path":    starPath(),
		"__builtin_json":    starjson.Module,
		"__builtin_time":    startime.Module,
		"__builtin_math":    starmath.Module,
		"__builtin_proto":   starproto.Module,
		"__builtin_struct":  starlark.NewBuiltin("struct", starlarkstruct.Make),
		"__builtin_module":  starlark.NewBuiltin("module", starlarkstruct.MakeModule),
	}
}
