# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __copy(ctx, cmd):
    input = cmd.inputs[0]
    out = cmd.outputs[0]
    ctx.actions.copy(input, out, recursive = ctx.fs.is_dir(input))
    ctx.actions.exit(exit_status = 0)

__handlers = {
    "copy": __copy,
}

def init(ctx):
    step_config = {
        "rules": [
            {
                "name": "simple/copy",
                "action": "copy",
                "handler": "copy",
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
