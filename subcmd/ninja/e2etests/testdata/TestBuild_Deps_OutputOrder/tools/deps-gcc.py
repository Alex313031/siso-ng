# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("-o", nargs="+")
  parser.add_argument("-MF", type=argparse.FileType(mode="w"))
  parser.add_argument("-c", type=argparse.FileType())
  options = parser.parse_args()

  for o in options.o:
    with open(o, "w") as f:
      f.write(options.c.read())
  options.MF.write(f"{options.o[0]}: {options.c.name} ../../base/foo.h\n")


if __name__ == "__main__":
  sys.exit(main())
