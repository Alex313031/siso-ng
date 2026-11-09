# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# genfile writes its input content into a single output file, used to
# exercise a target that is a file output (the same path the dir-output
# variant produces, to drive a file<->directory kind flip).

import argparse
import os
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out", required=True)
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  data = ""
  for input_path in options.inputs:
    with open(input_path) as f:
      data += f.read()

  # Clear any stale output (cross-platform replacement for a `rm -rf ${out} &&`
  # prefix): on a dir->file kind flip ${out} is a leftover directory, and siso
  # does not auto-remove a stale directory for a non-slash output, so the
  # producing rule must.
  if os.path.islink(options.out):
    os.remove(options.out)
  elif os.path.isdir(options.out):
    shutil.rmtree(options.out)
  elif os.path.exists(options.out):
    os.remove(options.out)

  parent = os.path.dirname(options.out)
  if parent:
    os.makedirs(parent, exist_ok=True)
  # newline='' so \n is not rewritten to \r\n on Windows.
  with open(options.out, "w", newline="") as f:
    f.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
