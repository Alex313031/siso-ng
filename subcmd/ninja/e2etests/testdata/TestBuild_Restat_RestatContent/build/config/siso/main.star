# Copyright 2025 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    return module(
        "config",
        step_config = json.encode({
            "rules": [
                {
                    "name": "action",
                    "action": "action",
                    "restat_content": True,
                },
            ],
        }),
        filegroups = {},
        handlers = {},
    )
