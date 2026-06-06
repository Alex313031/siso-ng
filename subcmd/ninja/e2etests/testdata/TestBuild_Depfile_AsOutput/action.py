# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("output", type=argparse.FileType(mode='w'))
  parser.add_argument("input", type=argparse.FileType())
  parser.add_argument("--dependency_out", type=argparse.FileType(mode='w'))
  options = parser.parse_args()

  options.output.write(options.input.read())
  options.dependency_out.write(f'{options.output.name}: {options.input.name}')
  return 0


if __name__ == "__main__":
  sys.exit(main())
