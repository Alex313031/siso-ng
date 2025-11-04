# Copyright 2024 The Chromium Authors
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
        "rules": [],
        "input_deps": {},
    }
    step_config["rules"].append({
        "name": "remote_action",
        "action": "remote_action",
        "remote": True,
        "reproxy_config": {
            "platform": {
                "container-image": "docker://gcr.io/test/test",
                "OSFamily": "Linux",
            },
            "labels": {"type": "tools"},
            "exec_strategy": "remote",
        },
    })

    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {},
    )
