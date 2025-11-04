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
  parser.add_argument("inputs", nargs='*')
  options = parser.parse_args()

  with zipfile.ZipFile(options.output, "w") as zf:
    for input in options.inputs:
      zf.write(input, arcname=os.path.basename(input))
  return 0


if __name__ == "__main__":
  sys.exit(main())
