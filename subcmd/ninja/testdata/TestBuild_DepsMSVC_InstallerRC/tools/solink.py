# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--output", help="output")
  parser.add_argument("--input", help="input")
  options = parser.parse_args()
  shutil.copyfile(options.input, options.output)


if __name__ == "__main__":
  sys.exit(main())
