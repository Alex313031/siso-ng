# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("-MF", type=argparse.FileType(mode='w'))
  parser.add_argument("-c", type=argparse.FileType())
  parser.add_argument("-o", type=argparse.FileType(mode='w'))
  options = parser.parse_args()

  options.o.write(options.c.read())
  options.MF.write(f'{options.o.name}: {options.c.name} foo.h foo.pb.h bar.h')
  return 0


if __name__ == "__main__":
  sys.exit(main())
