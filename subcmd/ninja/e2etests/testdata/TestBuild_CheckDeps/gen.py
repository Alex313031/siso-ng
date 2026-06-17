# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input", type=argparse.FileType())
  parser.add_argument("outs", type=argparse.FileType(mode='w'), nargs='+')
  options = parser.parse_args()

  data = options.input.read()
  for o in options.outs:
    o.write(data)

  return 0


if __name__ == "__main__":
  sys.exit(main())
