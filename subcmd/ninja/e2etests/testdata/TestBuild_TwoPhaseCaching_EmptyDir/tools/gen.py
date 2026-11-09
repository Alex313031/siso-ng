# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--output", required=True)
  parser.add_argument("--mkdir", required=True)
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  os.makedirs(options.mkdir, exist_ok=True)
  data = ""
  for inp in options.inputs:
    with open(inp) as f:
      data += f.read()
  with open(options.output, "w") as f:
    f.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
