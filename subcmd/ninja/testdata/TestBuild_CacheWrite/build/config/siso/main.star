# Copyright 2025 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.
load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    step_config = {
        "rules": [
            {
                "action": "action",
                "name": "action",
                "output_local": True,
                "platform": {
                    "container-image": "docker://gcr.io/test/test",
                },
                "timeout": "1s",
            },
            {
                "action": "gcc",
                "name": "gcc",
                "output_local": True,
                "platform": {
                    "container-image": "docker://gcr.io/test/test",
                },
                "timeout": "1s",
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {},
    )