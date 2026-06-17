# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//path.star", "path")
load("@builtin//struct.star", "module")

def __cxx(ctx, cmd):
    ctx.actions.fix(
        auxiliary_log_output_files = [
            # Do not call the output file "aux.out"! This is a reserved file
            # name on Windows 10 and lower, causing the test to fail on LUCI.
            ctx.fs.canonpath("./debug.out"),
            ctx.fs.canonpath("./aux_missing.out"),
        ],
        auxiliary_log_output_dirs = [
            ctx.fs.canonpath("./aux_dir"),
            ctx.fs.canonpath("./aux_missing_dir"),
        ],
    )

def init(ctx):
    step_config = {
        "platforms": {
            "default": {
                "OSFamily": "Linux",
            },
        },
        "rules": [
            {
                "name": "cxx",
                "action": "cxx",
                "handler": "__cxx",
                "remote": True,
                "platform_ref": "default",
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {
            "__cxx": __cxx,
        },
    )
