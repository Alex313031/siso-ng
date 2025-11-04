# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import subprocess
import sys


def main():
  subprocess.run(sys.argv[1:], check=True)
  return 0


if __name__ == "__main__":
  sys.exit(main())
