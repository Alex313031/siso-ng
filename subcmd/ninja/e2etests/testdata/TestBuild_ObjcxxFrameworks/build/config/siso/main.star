# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    filegroups = {}
    filegroups[ctx.fs.canonpath("./sdk/xcode_links/iPhoneSimulator.sdk") + ":headers"] = {
        "type": "glob",
        "includes": ["*"],
    }
    filegroups[ctx.fs.canonpath("./sdk/xcode_links/iPhoneSimulator.sdk/Developer/Library/Frameworks") + ":headers"] = {
        "type": "glob",
        "includes": ["*"],
    }
    filegroups[ctx.fs.canonpath("./sdk/xcode_links/iPhoneSimulator.platform/Developer/Library/Frameworks") + ":headers"] = {
        "type": "glob",
        "includes": ["*"],
    }
    step_config = {
        "platforms": {
            "default": {
                "OSFamily": "Linux",
                "container-image": "docker://gcr.io/test/test",
            },
        },
        "rules": [
            {
                "name": "objcxx",
                "action": "objcxx",
                "remote": True,
            },
        ],
        "input_deps": {},
    }

    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = filegroups,
        handlers = {},
    )
