# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("-R", action='store_true', help="recursive copy")
  parser.add_argument("src", help="copy source")
  parser.add_argument("dst", help="copy destination")
  options = parser.parse_args()

  if options.R:
    shutil.copytree(options.src, options.dst, dirs_exist_ok=True)
  else:
    shutil.copy(options.src, options.dst)


if __name__ == "__main__":
  sys.exit(main())
