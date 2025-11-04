#!/usr/bin/env python3
# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import shutil
import sys
import zipfile


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input", help="input zip filename")
  parser.add_argument("output_dir", help="output dir")
  options = parser.parse_args()

  if os.path.exists(options.output_dir):
    shutil.rmtree(options.output_dir)
  with zipfile.ZipFile(options.input) as zf:
    zf.extractall(options.output_dir)

  return 0


if __name__ == "__main__":
  sys.exit(main())
