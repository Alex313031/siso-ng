# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __stamp(ctx, cmd):
    out = cmd.outputs[0]
    ctx.actions.write(out)
    ctx.actions.exit(exit_status = 0)

__handlers = {
    "stamp": __stamp,
}

def init(ctx):
    step_config = {
        "rules": [
            {
                "name": "simple/stamp",
                "action": "stamp",
                "handler": "stamp",
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
