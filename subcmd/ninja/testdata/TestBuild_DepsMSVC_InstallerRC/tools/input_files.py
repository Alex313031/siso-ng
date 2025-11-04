# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--output", help="output")
  parser.add_argument("input", nargs='*', help="input")
  options = parser.parse_args()
  with open(options.output, 'w') as f:
    for fname in options.input:
      f.write(fname + '\n')


if __name__ == "__main__":
  sys.exit(main())
