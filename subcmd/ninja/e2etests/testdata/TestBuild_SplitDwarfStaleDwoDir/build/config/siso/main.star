# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

# Mirrors chromium's __clang_link handler in build/config/siso/clang_unix.star,
# which attaches <output_file>-dwo/ to a split-DWARF ThinLTO link's outputs.
def __clang_link(ctx, cmd):
    split_dwarf = False
    use_lto = False
    for arg in cmd.args:
        if arg == "-gsplit-dwarf":
            split_dwarf = True
        elif arg.startswith("-flto"):
            use_lto = True

    outputs = cmd.outputs
    reconcile_outputdirs = []
    if split_dwarf and use_lto:
        dwo_dir = cmd.outputs[0] + "-dwo/"
        outputs = cmd.outputs + [dwo_dir]
        reconcile_outputdirs = [dwo_dir]

    ctx.actions.fix(
        outputs = outputs,
        reconcile_outputdirs = reconcile_outputdirs,
    )

__handlers = {
    "clang_link": __clang_link,
}

def init(ctx):
    step_config = {
        "rules": [
            {
                "name": "clang/link",
                "action": "link",
                "handler": "clang_link",
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
