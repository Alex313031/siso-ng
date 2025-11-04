# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import os
import sys
import time

input = None
output = None
for arg in sys.argv:
  print(arg)
  if arg.startswith('--input='):
    input = arg[len('--input='):]
    print('input=' + input)
  if arg.startswith('--output='):
    output = arg[len('--output='):]
    print('output=' + output)

time.sleep(1)

if not output:
  print('no output')
  sys.exit(1)

with open(output, "w") as w:
  if input:
    with open(input) as r:
      w.write(r.read())
  print(os.path.abspath(output) + ' created')
