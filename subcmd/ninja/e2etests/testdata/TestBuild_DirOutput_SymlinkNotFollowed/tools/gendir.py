# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# gendir writes a regular file plus a symlink that points OUTSIDE the output
# directory (to an external dir). siso must capture the symlink as a symlink
# and must not follow it into the external dir when recording the output tree.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out_dir", required=True)
  parser.add_argument("--ext_dir", required=True)
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  data = ""
  for input_path in options.inputs:
    with open(input_path) as f:
      data += f.read()

  os.makedirs(options.out_dir, exist_ok=True)
  with open(os.path.join(options.out_dir, "data"), "w") as f:
    f.write(data)

  link = os.path.join(options.out_dir, "link")
  if os.path.lexists(link):
    os.remove(link)
  os.symlink(os.path.abspath(options.ext_dir), link)
  return 0


if __name__ == "__main__":
  sys.exit(main())
