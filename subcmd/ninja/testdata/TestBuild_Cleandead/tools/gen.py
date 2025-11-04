# Copyright 2023 The Chromium Authors
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

  with open(options.output, "w") as w:
    with open(options.input) as r:
      w.write(r.read())
  return 0


if __name__ == "__main__":
  sys.exit(main())
