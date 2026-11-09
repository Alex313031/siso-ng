# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# mkempty creates an empty output directory (no files inside it).

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out_dir", required=True)
  options = parser.parse_args()
  os.makedirs(options.out_dir, exist_ok=True)
  return 0


if __name__ == "__main__":
  sys.exit(main())
