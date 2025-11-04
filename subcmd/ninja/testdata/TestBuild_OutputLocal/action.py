# Copyright 2025 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--input", type=argparse.FileType(mode='rb'))
  parser.add_argument("--output", type=argparse.FileType(mode='wb'))
  parser.add_argument("--output1", type=argparse.FileType(mode='wb'))
  options = parser.parse_args()

  data = options.input.read()
  options.output.write(data)
  if options.output1:
    options.output1.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
