# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--output", required=True)
  parser.add_argument("--check-file", action="append", default=[])
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  data = ""
  for inp in options.inputs:
    with open(inp) as f:
      data += f.read()

  for cf in options.check_file:
    if not os.path.isfile(cf):
      print(f"missing file: {cf}", file=sys.stderr)
      return 1
    with open(cf) as f:
      got = f.read()
    if got != data:
      print(
        f"content mismatch for {cf}: got {got!r}, want {data!r}",
        file=sys.stderr,
      )
      return 1

  with open(options.output, "w") as f:
    f.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
