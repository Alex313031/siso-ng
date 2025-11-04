# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out", help="output")
  parser.add_argument("--inputs", help="input files")
  options = parser.parse_args()

  with open(options.inputs) as f:
    for line in f:
      file = line.strip()
      print('Note: including file: %s' % os.path.abspath(file))
  with open(options.out, 'w') as f:
    f.write(options.inputs)


if __name__ == "__main__":
  sys.exit(main())
