# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# Stands in for build/linux/dump_app_syms.py: fails on any .dwo in
# <output_file>-dwo/ whose dwo_id the binary does not reference.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("binary")
  parser.add_argument("--out", required=True)
  options = parser.parse_args()

  with open(options.binary) as f:
    referenced = {line.strip() for line in f if line.strip()}

  dwo_dir = options.binary + "-dwo"
  for name in sorted(os.listdir(dwo_dir)):
    if not name.endswith(".dwo"):
      continue
    with open(os.path.join(dwo_dir, name)) as f:
      dwo_id = f.read().strip()
    if dwo_id not in referenced:
      print(
        "dwo_id mismatch: %s/%s has %s, not referenced by %s"
        % (dwo_dir, name, dwo_id, options.binary),
        file=sys.stderr,
      )
      return 1

  with open(options.out, "w", newline="") as f:
    f.write("ok\n")
  return 0


if __name__ == "__main__":
  sys.exit(main())
