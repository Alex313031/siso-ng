# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __copy_bundle_data(ctx, cmd):
    input = cmd.inputs[0]
    out = cmd.outputs[0]
    print('copy_bundle_data %s to %s recursive=%s' % (input, out, ctx.fs.is_dir(input)))
    ctx.actions.copy(input, out, recursive = ctx.fs.is_dir(input))
    ctx.actions.exit(exit_status = 0)

def __stamp(ctx, cmd):
    out = cmd.outputs[0]
    ctx.actions.write(out)
    ctx.actions.exit(exit_status = 0)

__handlers = {
    "copy_bundle_data": __copy_bundle_data,
    "stamp": __stamp,
}

def init(ctx):
    return module(
        "config",
        step_config = json.encode({
            "rules": [
                {
		    "name": "stamp",
		    "action": "stamp",
		    "handler": "stamp",
		},
                {
                    "name": "copy_bundle_data",
                    "action": "copy_bundle_data",
                    "handler": "copy_bundle_data",
                },
            ],
        }),
        filegroups = {},
        handlers = __handlers,
    )
