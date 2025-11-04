# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("output", type=argparse.FileType(mode='w'))
  options = parser.parse_args()

  options.output.write('')
  return 0


if __name__ == "__main__":
  sys.exit(main())
