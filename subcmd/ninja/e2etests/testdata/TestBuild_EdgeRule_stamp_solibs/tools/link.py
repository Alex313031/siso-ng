# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# python3 ../../tools/link.py ${in} -o ${out}
# ${in} may be multiple

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument(
      "inputs", nargs='*', type=argparse.FileType(), help="inputs")
  parser.add_argument("-o", type=argparse.FileType(mode='w'), help="output")
  options = parser.parse_args()

  data = ''
  for input in options.inputs:
    data += input.name + '\n'
    data += input.read()
    data += '\n'
  options.o.write(data)


if __name__ == "__main__":
  sys.exit(main())
