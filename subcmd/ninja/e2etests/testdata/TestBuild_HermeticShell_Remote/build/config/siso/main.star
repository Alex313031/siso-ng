# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    step_config = {
        "shell": "bin/sh",
        "platforms": {
            "default": {
                "OSFamily": "Linux",
                "container-image": "docker://gcr.io/test/test",
            },
        },
        "rules": [
            {
                "name": "remote_nontrivial",
                "action": "remote_nontrivial",
                "remote": True,
            },
            {
                "name": "remote_trivial",
                "action": "remote_trivial",
                "remote": True,
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {},
    )
