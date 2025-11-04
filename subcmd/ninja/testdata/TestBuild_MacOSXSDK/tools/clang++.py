# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument(
      "-MMD", action='store_true', help="generate dependency file")
  parser.add_argument("-MF", help="dependenfy file")
  # -isysroot and -F are not used in this script,
  # but need to handle these flags on command line
  # so that siso can recognize them and add dir/tree for them.
  parser.add_argument("-isysroot", help="sysroot dir")
  parser.add_argument("-F", help="framework dir")

  parser.add_argument("-c", action='store_true', help="compile")
  parser.add_argument("-o", help="output file")
  parser.add_argument("input", help="input file")
  options = parser.parse_args()

  if options.MMD and options.MF:
    with open(options.MF, 'w') as f:
      f.write('%s: %s\n' % (options.o, options.input))
  shutil.copyfile(options.input, options.o)


if __name__ == "__main__":
  sys.exit(main())
