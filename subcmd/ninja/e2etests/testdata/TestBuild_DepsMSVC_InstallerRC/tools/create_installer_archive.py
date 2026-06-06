# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import shutil
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--staging_dir", help="staging dir")
  parser.add_argument("--resource_file_path", help="resource file path")
  parser.add_argument("input", help="input files")
  options = parser.parse_args()

  shutil.rmtree(options.staging_dir, ignore_errors=True)
  os.makedirs(options.staging_dir, mode=0o755, exist_ok=True)
  files = []
  with open(options.input) as f:
    for line in f:
      src = line.strip()
      srcdir = os.path.basename(os.path.dirname(src))
      dstdir = os.path.join(options.staging_dir, srcdir)
      os.makedirs(dstdir, mode=0o755, exist_ok=True)
      dst = os.path.join(dstdir, os.path.basename(src))
      print('copy %s to %s' % (src, dst))
      shutil.copyfile(src, dst)
      files.append(dst)
  with open(options.resource_file_path, 'w') as f:
    for file in files:
      f.write(file + '\n')
  return 0


if __name__ == "__main__":
  sys.exit(main())
