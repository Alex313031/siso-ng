# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("stamp")
  options = parser.parse_args()
  with open(options.stamp, "w") as w:
    w.write("")
  return 0


if __name__ == "__main__":
  sys.exit(main())
