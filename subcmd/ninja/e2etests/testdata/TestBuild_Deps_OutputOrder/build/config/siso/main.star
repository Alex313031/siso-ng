# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __reorder(ctx, cmd):
    # b/558995453: simulate output order issue where cmd.outputs order
    # differs from ninja edge outputs (e.g. 2PC or REAPI sorting).
    outputs = [cmd.outputs[i] for i in range(len(cmd.outputs) - 1, -1, -1)]
    ctx.actions.fix(outputs = outputs)

__handlers = {
    "reorder": __reorder,
}

def init(ctx):
    step_config = {
        "platforms": {
            "default": {
                "OSFamily": "Linux",
                "container-image": "docker://gcr.io/test/test",
            },
        },
        "rules": [
            {
                "name": "gcc",
                "action": "gcc",
                "handler": "reorder",
                "remote": True,
            },
        ],
        "input_deps": {},
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
