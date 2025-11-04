# Copyright 2025 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys
import shutil


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input_dir", help="input dir")
  parser.add_argument("output_dir", help="output dir")
  options = parser.parse_args()

  if os.path.exists(options.output_dir):
    print("remove %s" % options.output_dir)
    shutil.rmtree(options.output_dir)
  print("makedir %s" % options.output_dir)
  os.makedirs(options.output_dir, 0o755)
  with open(os.path.join(options.input_dir, "input")) as f:
    for line in f:
      if line.startswith("#"):
        continue
      line = line.strip()
      src = os.path.join(options.input_dir, line)
      if os.path.exists(src):
        print("copy %s" % line)
        shutil.copy(src, os.path.join(options.output_dir, line))
      else:
        print("not exist %s" % line)


if __name__ == "__main__":
  sys.exit(main())
