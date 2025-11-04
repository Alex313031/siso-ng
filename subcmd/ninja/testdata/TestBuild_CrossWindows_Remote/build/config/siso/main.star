# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __run_prog(ctx, cmd):
    inputs = []
    for arg in cmd.args[1:3]:
        inputs.append(ctx.fs.canonpath(arg))
    print("inputs=%s" % inputs)
    ctx.actions.fix(cmd.inputs + inputs)

__handlers = {
    "run_prog": __run_prog,
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
                "name": "action",
                "action": "action",
                "handler": "run_prog",
                "remote": True,
            },
        ],
        "executables": [
            "tools/cp",
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
