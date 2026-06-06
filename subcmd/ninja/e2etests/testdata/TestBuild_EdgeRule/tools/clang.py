# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# python3 ../../tools/clang.py -c {$in} -o ${out}

import argparse
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("-c", help="compile")
  parser.add_argument("-o", help="output")
  options = parser.parse_args()

  shutil.copyfile(options.c, options.o)


if __name__ == "__main__":
  sys.exit(main())
