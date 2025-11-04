# Copyright 2025 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input", type=argparse.FileType())
  parser.add_argument("output", type=argparse.FileType(mode='w'))
  options = parser.parse_args()

  options.output.write(options.input.read())
  return 0


if __name__ == "__main__":
  sys.exit(main())
