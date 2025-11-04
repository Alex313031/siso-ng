# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys
import time


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument('target', help='target filename')
  parser.add_argument('link_name', help='link filename')
  options = parser.parse_args()
  time.sleep(0.2)
  os.symlink(options.target, options.link_name)


if __name__ == '__main__':
  sys.exit(main())
