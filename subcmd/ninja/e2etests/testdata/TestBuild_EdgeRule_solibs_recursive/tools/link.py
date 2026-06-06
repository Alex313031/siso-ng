# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# python3 ../../tools/link.py ${in} -o ${out}
# ${in} may be multiple

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("inputs", nargs='*', help="inputs")
  parser.add_argument("-o", nargs='*', help="output")
  options = parser.parse_args()

  data = ''
  for input in options.inputs:
    data += input + '\n'
    with open(input) as f:
      data += f.read()
      data += '\n'
  for output in options.o:
    with open(output, 'w') as f:
      f.write(data)


if __name__ == "__main__":
  sys.exit(main())
