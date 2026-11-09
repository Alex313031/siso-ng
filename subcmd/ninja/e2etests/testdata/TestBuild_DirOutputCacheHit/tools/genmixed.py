# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# genmixed produces BOTH a file output (--out) and a directory output
# (--out_dir containing inner files). It is the mixed file+dir output
# scenario used to exercise the action-cache-hit path.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out", required=True)
  parser.add_argument("--out_dir", required=True)
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  data = ""
  for input_path in options.inputs:
    with open(input_path) as f:
      data += f.read()

  # newline='' so \n is not rewritten to \r\n on Windows; the build captures
  # the bytes verbatim, and the test compares exact content.
  with open(options.out, "w", newline="") as f:
    f.write("FILE:" + data)
  os.makedirs(os.path.join(options.out_dir, "sub"), exist_ok=True)
  with open(os.path.join(options.out_dir, "inner.txt"), "w", newline="") as f:
    f.write("INNER:" + data)
  with open(
    os.path.join(options.out_dir, "sub", "nested.txt"), "w", newline=""
  ) as f:
    f.write("NESTED:" + data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
