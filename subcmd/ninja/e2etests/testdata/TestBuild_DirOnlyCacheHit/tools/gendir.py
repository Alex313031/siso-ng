# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# gendir creates an EMPTY output directory (no files inside it). The
# action's only output is a directory, and that directory has no files,
# so the action result has no OutputFiles -- only an OutputDirectory.
# This is the case that exercises validateRemoteActionResult accepting a
# directory-only result (expanding the tree adds no OutputFiles to fall
# back on).

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out_dir", required=True)
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()
  # Read inputs so the action depends on them (cache key), but produce
  # only an empty directory.
  for input_path in options.inputs:
    with open(input_path) as f:
      f.read()
  os.makedirs(options.out_dir, exist_ok=True)
  return 0


if __name__ == "__main__":
  sys.exit(main())
