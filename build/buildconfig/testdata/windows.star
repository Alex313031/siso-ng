# -*- bazel-starlark -*-
# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//struct.star", "module")

__filegroups = {}

__handlers = {}

def __step_config(ctx):
    step_config = {}
    return step_config

chromium = module(
    "chromium",
    step_config = __step_config,
    filegroups = __filegroups,
    handlers = __handlers,
)
