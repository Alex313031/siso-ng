# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input")
  parser.add_argument("output")
  options = parser.parse_args()

  data = ""
  with open(options.input) as r:
    data = r.read()
  with open(options.output, "w") as w:
    w.write(data)
    for i in range(0, 1024 * 1024):
      w.write(' ')
  return 0


if __name__ == "__main__":
  sys.exit(main())
