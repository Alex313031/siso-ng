# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# gendir reads a manifest file (one output-relative path per line) and creates
# exactly those files inside the output directory. It does NOT clear the output
# directory itself, so a rerun with a shorter manifest only removes stale files
# if siso wipes the directory output before running the command.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out_dir", required=True)
  parser.add_argument("manifest")
  options = parser.parse_args()

  with open(options.manifest) as f:
    names = [line.strip() for line in f if line.strip()]

  os.makedirs(options.out_dir, exist_ok=True)
  for name in names:
    path = os.path.join(options.out_dir, name)
    parent = os.path.dirname(path)
    if parent:
      os.makedirs(parent, exist_ok=True)
    # newline='' so \n is not rewritten to \r\n on Windows.
    with open(path, "w", newline="") as g:
      g.write("content\n")
  return 0


if __name__ == "__main__":
  sys.exit(main())
