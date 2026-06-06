# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __stamp(ctx, cmd):
    ctx.actions.write(cmd.outputs[0])
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
            {
                "name": "action",
                "action": "action",
                "remote": True,
                "reproxy_config": {
                    "platform": {
                        "container-image": "gcr.io/test/test",
                    },
                    "labels": {
                        "type": "tool",
                    },
                    "exec_strategy": "remote_local_fallback",
                    "exec_timeout": "1m",
                    "reclient_timeout": "1m",
                },
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
