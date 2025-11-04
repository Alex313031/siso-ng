# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    return module(
        "config",
        step_config = json.encode({
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
                    "remote": True,
                },
            ],
        }),
        filegroups = {},
        handlers = {},
    )
