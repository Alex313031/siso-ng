# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# gendir creates a directory output with one file and writes a depfile for it.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out_dir", required=True)
  parser.add_argument("--depfile", required=True)
  options = parser.parse_args()
  os.makedirs(options.out_dir, exist_ok=True)
  with open(os.path.join(options.out_dir, "data.txt"), "w") as f:
    f.write("data\n")
  with open(options.depfile, "w") as f:
    f.write("%s/: ../../tools/gendir.py\n" % options.out_dir)
  return 0


if __name__ == "__main__":
  sys.exit(main())
