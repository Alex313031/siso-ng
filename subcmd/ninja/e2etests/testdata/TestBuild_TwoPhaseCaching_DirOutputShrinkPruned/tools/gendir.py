# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# gendir reads a manifest file (one output-relative path per line) and creates
# exactly those files inside the output directory. It does NOT clear the output
# directory itself.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--output", required=True)
  parser.add_argument("manifest")
  options = parser.parse_args()

  with open(options.manifest) as f:
    names = [line.strip() for line in f if line.strip()]

  os.makedirs(options.output, exist_ok=True)
  for name in names:
    path = os.path.join(options.output, name)
    # newline='' so \n is not rewritten to \r\n on Windows.
    with open(path, "w", newline="") as g:
      g.write("content-" + name + "\n")
  return 0


if __name__ == "__main__":
  sys.exit(main())
