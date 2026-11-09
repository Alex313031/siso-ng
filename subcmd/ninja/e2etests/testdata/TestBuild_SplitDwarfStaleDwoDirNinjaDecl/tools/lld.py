# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# Stands in for a split-DWARF ThinLTO link: writes the binary listing the
# dwo_ids it references, plus one .dwo per unit into <output_file>-dwo/. Like
# the real LLD it never clears that directory.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out", required=True)
  parser.add_argument("manifest")
  # The -gsplit-dwarf / -flto flags the handler keys on are passed through.
  options, _ = parser.parse_known_args()

  with open(options.manifest) as f:
    units = [line.strip() for line in f if line.strip()]

  # newline='' so \n is not rewritten to \r\n on Windows.
  with open(options.out, "w", newline="") as f:
    for unit in units:
      f.write("dwo_id=%s\n" % unit)

  dwo_dir = options.out + "-dwo"
  os.makedirs(dwo_dir, exist_ok=True)
  for unit in units:
    with open(os.path.join(dwo_dir, unit + ".dwo"), "w", newline="") as f:
      f.write("dwo_id=%s\n" % unit)
  return 0


if __name__ == "__main__":
  sys.exit(main())
