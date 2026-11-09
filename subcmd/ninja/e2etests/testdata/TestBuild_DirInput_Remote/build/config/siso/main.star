# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    step_config = {
        "platforms": {
            "default": {
                "OSFamily": "Linux",
            },
        },
        "rules": [
            {
                "name": "unzip",
                "action": "unzip",
                "remote": True,
                "platform_ref": "default",
            },
            {
                "name": "mkzip",
                "action": "mkzip",
                "remote": True,
                "platform_ref": "default",
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {},
    )
