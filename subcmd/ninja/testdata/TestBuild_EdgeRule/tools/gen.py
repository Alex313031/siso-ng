# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input", help="generator input")
  parser.add_argument("outputs", nargs='*', help="generator output")
  options = parser.parse_args()

  with open(options.input) as f:
    data = f.read()
  for output in options.outputs:
    with open(output, 'w') as f:
      f.write(data)


if __name__ == "__main__":
  sys.exit(main())
