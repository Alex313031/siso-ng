# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input", help="input file")
  parser.add_argument("output", help="output file")
  options = parser.parse_args()
  with open(options.input) as r:
    data = r.read()
  with open(options.output, 'w') as w:
    w.write(data)


if __name__ == "__main__":
  sys.exit(main())
