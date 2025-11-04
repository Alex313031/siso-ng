# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# python3 ../../protobuf/protoc_wrapper.py ${protoc} ${in} ${out}
# ${out} may be multiple

import argparse
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("protoc", help="protoc binary")
  parser.add_argument("input", help="input")
  parser.add_argument("outputs", nargs="*", help="outputs")
  options = parser.parse_args()

  for output in options.outputs:
    with open(output, 'w') as f:
      f.write('generate %s from %s' % (output, options.input))


if __name__ == "__main__":
  sys.exit(main())
