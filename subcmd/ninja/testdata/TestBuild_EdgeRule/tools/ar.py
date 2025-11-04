# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# python3 ../../tools/ar.py -c ${out} ${in}
# ${in} may be multiple.

import argparse
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("-c", help="create archive")
  parser.add_argument("inputs", nargs='*', help="inputs")
  options = parser.parse_args()

  data = ''
  for input in options.inputs:
    data += input + '\n'
  with open(options.c, 'w') as f:
    f.write(data)


if __name__ == "__main__":
  sys.exit(main())
