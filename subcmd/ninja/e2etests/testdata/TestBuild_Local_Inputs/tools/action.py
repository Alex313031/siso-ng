# Copyright 2025 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out-dir", help="output directory")
  parser.add_argument(
      "inputs", type=argparse.FileType(), nargs='+', help="input files")

  options = parser.parse_args()
  for input in options.inputs:
    output = os.path.normpath(
        os.path.join(options.out_dir, "out/siso", input.name))
    with open(output, mode='w') as w:
      w.write(input.read())


if __name__ == "__main__":
  sys.exit(main())
