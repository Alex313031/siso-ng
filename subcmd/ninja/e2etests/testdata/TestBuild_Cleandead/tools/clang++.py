# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("-MF", help="deps filename")
  parser.add_argument("-o", help="output filename")
  parser.add_argument("-c", help="compile", action='store_true')
  parser.add_argument("inputs", nargs='*')
  options = parser.parse_args()

  if options.c:
    with open(options.o, "w") as f:
      f.write("compile result of %s" % options.inputs)
    if options.MF:
      with open(options.MF, "w") as f:
        f.write("%s:")
        for input in options.inputs:
          f.write(" %s" % input)
    return 0
  with open(options.o, "w") as f:
    f.write("link result of %s" % options.inputs)
  return 0


if __name__ == "__main__":
  sys.exit(main())
