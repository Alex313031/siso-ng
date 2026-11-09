# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# stamp concatenates the contents of all regular files under <in_dir> into the
# output. It is the downstream consumer of the directory output; it must be
# skipped on rebuild when the directory's content is unchanged. os.walk does
# not follow symlinks, so the external dir behind gen/link is not read here.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument("--out", required=True)
  parser.add_argument("in_dir")
  options = parser.parse_args()

  data = ""
  for root, _, files in os.walk(options.in_dir):
    for name in sorted(files):
      with open(os.path.join(root, name)) as f:
        data += f.read()

  with open(options.out, "w") as f:
    f.write(data)
  return 0


if __name__ == "__main__":
  sys.exit(main())
