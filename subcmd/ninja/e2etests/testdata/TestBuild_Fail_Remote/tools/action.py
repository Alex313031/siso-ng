# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input")
  parser.add_argument("output")
  options = parser.parse_args()

  data = ""
  with open(options.input) as r:
    data = r.read()

  # error
  if "error" in data:
    sys.stderr.write("error\n")
    return 1
  if os.path.exists(options.output):
    oldData = ""
    with open(options.output) as r:
      oldData = r.read()

    # for restat
    if data == oldData:
      return 0
  with open(options.output, "w") as w:
    w.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
