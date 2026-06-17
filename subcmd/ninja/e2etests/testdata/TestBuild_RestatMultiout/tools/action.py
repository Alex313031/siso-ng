# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys
import time


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument('--output', help='output file', required=True)
  parser.add_argument('inputs', nargs='*')
  options = parser.parse_args()

  data = ''
  for input in options.inputs:
    with open(input) as f:
      data += f.read()

  time.sleep(0.1)
  with open(options.output, 'w') as f:
    f.write(data)
  return 0


if __name__ == '__main__':
  sys.exit(main())
