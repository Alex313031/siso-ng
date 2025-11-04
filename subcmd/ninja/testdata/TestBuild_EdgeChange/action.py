# Copyright 2025 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import re
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--input", type=argparse.FileType())
  parser.add_argument("--output", type=argparse.FileType(mode='w'))
  options = parser.parse_args()

  data = re.subn(r'#.*\n', '', options.input.read())[0]
  if os.path.exists("../../in2"):
    with open("../../in2") as f:
      data += re.subn(r'#.*\n', '', f.read())[0]
  if os.path.exists("../../in3"):
    with open("../../in3") as f:
      data += re.subn(r'#.*\n', '', f.read())[0]
  options.output.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
