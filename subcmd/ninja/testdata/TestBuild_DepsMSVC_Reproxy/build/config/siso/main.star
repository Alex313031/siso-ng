# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//struct.star", "module")
load("@builtin//encoding.star", "json")

def __cxx(ctx, cmd):
    reproxy_config = {
        "platform": {"container-image": "gcr.io/xxx"},
        "labels": {"type": "compile", "compiler": "clang-cl", "lang": "cpp"},
        "exec_strategy": "remote",
    }
    print(json.encode(reproxy_config))
    ctx.actions.fix(reproxy_config = json.encode(reproxy_config))

__handlers = {
    "cxx": __cxx,
}

def __step_config(ctx):
    step_config = {}
    step_config["rules"] = [
        {
            "name": "cxx",
            "action": "cxx",
            "handler": "cxx",
            "debug": True,
        },
    ]
    return step_config

def init(ctx):
    return module(
        "config",
        step_config = json.encode(__step_config(ctx)),
        filegroups = {},
        handlers = __handlers,
    )
