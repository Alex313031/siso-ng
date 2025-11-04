# -*- bazel-starlark -*-
# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//lib/gn.star", "gn")
load("@builtin//struct.star", "module")

def init(ctx):
    gn_args = gn.args(ctx)
    if gn_args.get("target_os") != '"chromeos"':
        fail("target_os=%s want=%s" % (gn_args.get("target_os"), '"chromeos"'))
    if gn_args.get("enable_nacl") != "false":
        fail("enable_nacl=%s want=%s" % (gn_args.get("enable_nacl"), "false"))
    if  gn_args.get("use_siso") != "true":
        fail("use_siso=%s want=%s" % (gn_args.get("use_siso"), "true"))
    if gn_args.get("use_remoteexec") != "true":
        fail("use_remoteexec=%s want=%s" % (gn_args.get("use_remoteexec"), "true"))
    # use_goma is not set as it is in comment.
    if gn_args.get("use_goma"):
        fail("use_goma=%s want=%s" % (gn_args.get("use_goma"), ""))
    return module(
        "gn_test",
        step_config = "",
        filegroups = {},
        handlers = {},
    )
