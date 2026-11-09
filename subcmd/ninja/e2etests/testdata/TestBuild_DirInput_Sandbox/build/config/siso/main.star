# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    step_config = {
        "sandbox": {
            "backend": "nsjail",
            "nsjail_path": "../../nsjail",
            "nsjail_workdir": ".nsjail_workdir",
            "nsjail_outdir": "out/siso",
        },
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {},
    )
