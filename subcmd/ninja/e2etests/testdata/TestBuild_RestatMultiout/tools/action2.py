# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import sys


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument('--output', help='output file', required=True)
  parser.add_argument('inputs', nargs='*')
  options = parser.parse_args()

  data = ''
  for input in options.inputs:
    print('input=%s' % input)
    with open(input) as f:
      data += f.read()

  # first output becomes dirty,
  # but mtime will be older than next step's output.
  first = True
  for output in options.output.split(' '):
    print('output=%s' % output)
    if first and os.path.exists(output):
      st = os.stat(output)
      with open(output, 'w') as f:
        f.write('updated')
      os.utime(output, times=(st.st_atime, st.st_mtime + 0.001))
      first = False
      continue
    first = False
    with open(output, 'w') as f:
      f.write(data)
  return 0


if __name__ == '__main__':
  sys.exit(main())
