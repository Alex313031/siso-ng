#!/usr/bin/env python3
# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys
import zipfile


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("output", help="output zip filename")
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  with zipfile.ZipFile(options.output, "w") as zf:
    for input_path in options.inputs:
      zf.write(input_path, arcname=os.path.basename(input_path))
  return 0


if __name__ == "__main__":
  sys.exit(main())
