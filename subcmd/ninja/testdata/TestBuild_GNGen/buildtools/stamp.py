# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument('--input', help='input')
  parser.add_argument('--output', help='output')
  options = parser.parse_args()

  data = ''
  with open(options.input) as f:
    data = f.read()
  with open(options.output, 'w') as f:
    f.write('input %s' % data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
