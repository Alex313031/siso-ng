# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--output", action="append", default=[])
  parser.add_argument("--unlink-output", action="append", default=[])
  parser.add_argument("--temp-file")
  parser.add_argument("inputs", nargs="*")
  options = parser.parse_args()

  data = ""
  for inp in options.inputs:
    with open(inp) as f:
      data += f.read()

  if options.temp_file:
    with open(options.temp_file, "w") as f:
      f.write("temp")
    os.remove(options.temp_file)

  for out in options.unlink_output:
    if os.path.exists(out):
      os.remove(out)
    with open(out, "w") as f:
      f.write(data)

  for out in options.output:
    with open(out, "w") as f:
      f.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
