# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# gendir writes its input content into an output directory tree, used to
# exercise a target that is a directory output.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out_dir", required=True)
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  data = ""
  for input_path in options.inputs:
    with open(input_path) as f:
      data += f.read()

  os.makedirs(os.path.join(options.out_dir, "sub"), exist_ok=True)
  # newline='' so \n is not rewritten to \r\n on Windows; the test compares
  # exact content.
  with open(os.path.join(options.out_dir, "data"), "w", newline="") as f:
    f.write(data)
  with open(
    os.path.join(options.out_dir, "sub", "nested"), "w", newline=""
  ) as f:
    f.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
