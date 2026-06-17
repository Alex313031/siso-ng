# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("-r", action='store_true')
  parser.add_argument("input", type=argparse.FileType())
  parser.add_argument("output", type=argparse.FileType(mode='w'))
  options = parser.parse_args()

  data = options.input.read()
  if options.r:
    data = data[::-1]
  options.output.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
