# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import sys
import time


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--sleep", type=float, default=0, help="sleep seconds")
  parser.add_argument("input", type=argparse.FileType(), help="input file")
  parser.add_argument("output", type=argparse.FileType("w"), help="output file")
  options = parser.parse_args()
  if options.sleep:
    time.sleep(options.sleep)
  options.output.write("")
  return 0


if __name__ == "__main__":
  sys.exit(main())
