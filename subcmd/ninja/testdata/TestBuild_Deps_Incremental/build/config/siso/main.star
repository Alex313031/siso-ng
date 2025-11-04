# Copyright 2025 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

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
                "remote": True,
            },
            {
                "name": "cl",
                "action": "cl",
                "remote": True,
            },
            {
                "name": "action",
                "action": "action",
                "remote": True,
            },
        ],
        "input_deps": {},
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {},
    )
