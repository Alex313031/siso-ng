# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# copytree copies a source directory to the output directory with
# shutil.copytree, which REQUIRES the destination to not already exist. It
# stands in for any action (cp -r, rsync, archive extraction) that creates its
# own output directory. If siso pre-created the output directory, copytree
# raises FileExistsError: the local/remote divergence this guards against, since
# a REAPI worker never creates the output directory itself.

import argparse
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out_dir", required=True)
  parser.add_argument("src")
  options = parser.parse_args()

  shutil.copytree(options.src, options.out_dir)
  return 0


if __name__ == "__main__":
  sys.exit(main())
