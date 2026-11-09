# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")
def __copy_handler(ctx, cmd):
    ctx.actions.copy(cmd.inputs[0], cmd.outputs[0])
    ctx.actions.exit(exit_status=0)

def init(ctx):
    step_config = {
        "rules": [
            {
                "name": "copy_rule",
                "action": "copy_rule",
                "handler": "copy_handler",
            },
        ]
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {
            "copy_handler": __copy_handler,
        },
    )
