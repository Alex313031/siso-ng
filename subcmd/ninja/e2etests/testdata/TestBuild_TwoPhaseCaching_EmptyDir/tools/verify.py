# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--output", required=True)
  parser.add_argument("--check-dir", required=True)
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  if not os.path.isdir(options.check_dir):
    print(f"missing directory: {options.check_dir}", file=sys.stderr)
    return 1

  data = ""
  for inp in options.inputs:
    with open(inp) as f:
      data += f.read()
  with open(options.output, "w") as f:
    f.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
