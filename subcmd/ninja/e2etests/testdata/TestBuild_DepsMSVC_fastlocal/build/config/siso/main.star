# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    step_config = {
        "platforms": {
            "default": {
                "OSFamily": "Linux",
                "container-image": "docker://gcr.io/chops-public-images-prod/rbe/siso-chromium/linux@sha256:912808c295e578ccde53b0685bcd0d56c15d7a03e819dcce70694bfe3fdab35e",
            },
        },
        "input_deps": {},
        "rules": [
            {
                "name": "clang/cxx",
                "action": "cxx",
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
