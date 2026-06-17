# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--provision", action='store_true')
  parser.add_argument("path")
  options = parser.parse_args()

  provision_path = os.path.join(options.path, "provision")
  if os.path.exists(provision_path):
    os.unlink(provision_path)
  if options.provision:
    with open(provision_path, "w") as w:
      w.write("provision")

  with open(os.path.join(options.path, "xcuitests_module"), "w") as w:
    w.write("xcuitests_module")

  return 0


if __name__ == "__main__":
  sys.exit(main())
