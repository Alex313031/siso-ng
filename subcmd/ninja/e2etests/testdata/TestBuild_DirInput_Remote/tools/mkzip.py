#!/usr/bin/env python3
# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys
import zipfile


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("output", help="output zip filename")
  parser.add_argument("input_dir", help="input dir")
  options = parser.parse_args()

  with zipfile.ZipFile(options.output, "w") as zf:
    for root, dirs, files in os.walk(options.input_dir):
      dirs.sort()
      for f in sorted(files):
        filepath = os.path.join(root, f)
        arcname = os.path.relpath(filepath, options.input_dir)
        zf.write(filepath, arcname=arcname)

  return 0


if __name__ == "__main__":
  sys.exit(main())
