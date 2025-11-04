# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __copy_bundle_data(ctx, cmd):
    input = cmd.inputs[0]
    out = cmd.outputs[0]
    ctx.actions.copy(input, out, recursive = ctx.fs.is_dir(input))
    ctx.actions.exit(exit_status = 0)

__handlers = {
    "copy_bundle_data": __copy_bundle_data,
}

def init(ctx):
    step_config = {
        "rules": [
            {
                "name": "simple/copy_bundle_data",
                "action": "copy_bundle_data",
                "handler": "copy_bundle_data",
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
