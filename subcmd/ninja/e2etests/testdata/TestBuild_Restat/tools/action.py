# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument('--output', help='output file', required=True)
  parser.add_argument('inputs', nargs='*')
  options = parser.parse_args()

  data = ''
  for input in options.inputs:
    with open(input) as f:
      data += f.read()

  if os.path.exists(options.output):
    with open(options.output) as f:
      current = f.read()

    if data == current:
      return 0

  with open(options.output, 'w') as f:
    f.write(data)
  return 0


if __name__ == '__main__':
  sys.exit(main())
