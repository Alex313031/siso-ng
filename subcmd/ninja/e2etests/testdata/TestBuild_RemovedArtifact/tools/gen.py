# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import os
import sys

input_path = None
output = None
for arg in sys.argv:
  print(arg)
  if arg.startswith("--input="):
    input_path = arg[len("--input=") :]
    print("input=" + input_path)
  if arg.startswith("--output="):
    output = arg[len("--output=") :]
    print("output=" + output)

if not output:
  print("no output")
  sys.exit(1)

with open(output, "w") as w:
  if input_path:
    with open(input_path) as r:
      w.write(r.read())
  print(os.path.abspath(output) + " created")
