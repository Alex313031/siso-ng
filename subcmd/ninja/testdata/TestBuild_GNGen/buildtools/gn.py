# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import argparse
import os
import shutil
import sys

SCRIPT_DIR = os.path.dirname(os.path.realpath(__file__))


def main():
  parser = argparse.ArgumentParser()
  parser.add_argument('--root', help='root dir')
  parser.add_argument('-q', help='quiet', action='store_true')
  parser.add_argument(
      '--regeneration', help='regenerate build.ninja', action='store_true')
  # cmd: gen / clean only
  parser.add_argument('cmd')
  parser.add_argument('outdir')
  options = parser.parse_args()

  if options.cmd == 'gen':
    os.makedirs(options.outdir, exist_ok=True)
    if not options.regeneration:
      with open(os.path.join(options.outdir, 'args.gn'), 'w') as f:
        f.write("""
# Set build arguments here. See `gn help buildargs`.
is_debug=false
        """)
    # generates
    with open(os.path.join(options.outdir, 'build.ninja'), 'w') as f:
      with open(os.path.join(SCRIPT_DIR, "gn_data/build.ninja")) as r:
        f.write(r.read())
    with open(os.path.join(options.outdir, 'build.ninja.d'), 'w') as f:
      f.write('build.ninja.stamp: ../../.gn ../../BUILD.gn\n')
    with open(os.path.join(options.outdir, 'toolchain.ninja'), 'w') as f:
      with open(os.path.join(SCRIPT_DIR, "gn_data/toolchain.ninja")) as r:
        f.write(r.read())
    with open(os.path.join(options.outdir, 'build.ninja.stamp'), 'w') as f:
      pass

  elif options.cmd == 'clean':
    args = ''
    with open(os.path.join(options.outdir, 'args.gn')) as f:
      args = f.read()
    shutil.rmtree(options.outdir)
    os.makedirs(options.outdir, exist_ok=True)
    with open(os.path.join(options.outdir, 'args.gn'), 'w') as f:
      f.write(args)
    with open(os.path.join(options.outdir, 'build.ninja'), 'w') as f:
      with open(os.path.join(SCRIPT_DIR, "gn_data/build.ninja.clean")) as r:
        f.write(r.read())
    with open(os.path.join(options.outdir, 'build.ninja.d'), 'w') as f:
      f.write('build.ninja.stamp: nonexistent_file.gn\n')
  else:
    sys.stderr.write('unsupported command %s' % options.cmd)
    return 1
  return 0


if __name__ == "__main__":
  sys.exit(main())
