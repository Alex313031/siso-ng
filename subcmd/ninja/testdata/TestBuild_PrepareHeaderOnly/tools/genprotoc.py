# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  with open("protoc.py", "w") as f:
    f.write("""

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--python", action='store_true')
  parser.add_argument("input", type=argparse.FileType())
  options = parser.parse_args()

  if options.python:
    with open("gen/foo_pb.h", "w") as f:
        f.write("")
    return 0
  with open("gen/bar.pb.h", "w") as f:
      f.write("")
  with open("gen/bar.pb.cc", "w") as f:
      f.write("")
  return 0


if __name__ == "__main__":
  sys.exit(main())
""")
  return 0


if __name__ == "__main__":
  sys.exit(main())
