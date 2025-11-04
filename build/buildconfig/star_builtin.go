// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"embed"
	"os/exec"
	"runtime"
	"strings"

	starjson "go.starlark.net/lib/json"
	starmath "go.starlark.net/lib/math"
	starproto "go.starlark.net/lib/proto"
	startime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/runtimex"
)

// embeds these Starlark files for @builtin.
//
//go:embed checkout.star encoding.star path.star runtime.star struct.star lib/gn.star
var builtinStar embed.FS

func builtinModule(ctx context.Context) map[string]starlark.Value {
	runtimeModule := &starlarkstruct.Module{
		Name: "runtime",
		Members: map[string]starlark.Value{
			"num_cpu": starlark.MakeInt(runtimex.NumCPU()),
			"os":      starlark.String(runtime.GOOS),
			"arch":    starlark.String(runtime.GOARCH),
			// need to include os version (to select platform container images)?
		},
	}
	runtimeModule.Freeze()

	checkoutModule := &starlarkstruct.Module{
		Name:    "checkout",
		Members: map[string]starlark.Value{},
	}
	var origin string
	cmd := exec.Command("git", "ls-remote", "--get-url", "origin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		clog.Warningf(ctx, "failed to get git origin url: %v\n%s", err, out)
	} else {
		origin = strings.TrimSpace(string(out))
		clog.Infof(ctx, "git.origin=%q", origin)
	}
	checkoutModule.Members["git"] = starlarkstruct.FromStringDict(
		starlark.String("git"),
		map[string]starlark.Value{
			"origin": starlark.String(origin),
		},
	)
	checkoutModule.Freeze()

	return map[string]starlark.Value{
		"__builtin_runtime":  runtimeModule,
		"__builtin_checkout": checkoutModule,
		"__builtin_path":     starPath(),
		"__builtin_json":     starjson.Module,
		"__builtin_time":     startime.Module,
		"__builtin_math":     starmath.Module,
		"__builtin_proto":    starproto.Module,
		"__builtin_struct":   starlark.NewBuiltin("struct", starlarkstruct.Make),
		"__builtin_module":   starlark.NewBuiltin("module", starlarkstruct.MakeModule),
	}
}
