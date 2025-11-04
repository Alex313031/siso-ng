# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("input")
  options = parser.parse_args()

  if options.input == "foo":
    if not os.path.exists('gen/absl_status.mojom-module'):
      sys.stderr.write('file not found: gen/absl_status.mojom-module')
      return 1
    if not os.path.exists('gen/base.build_metadata'):
      sys.stderr.write('file not found: gen/base.build_metadata')
      return 1
    with open("foo.mojom-module", "w") as f:
      f.write("foo.mojom-module")
  elif options.input == "absl_status":
    if not os.path.exists('gen/base.build_metadata'):
      sys.stderr.write('file not found: gen/base.build_metadata')
      return 1
    with open("gen/absl_status.mojom-module", "w") as f:
      f.write("absl_status.mojom-module")
  else:
    sys.stderr.write(f'invalid input %{options.input}')
    return 1
  return 0


if __name__ == "__main__":
  sys.exit(main())
