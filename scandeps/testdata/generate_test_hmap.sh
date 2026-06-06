#!/bin/sh
# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# generate hmap test data using chromium's
# build/config/ios/write_framework_hmap.py

set -e
this_dir="$(cd $(dirname "$0"); pwd)"

# expected to run this on MacOSX. /tmp -> /private/tmp
if [[ "$(uname)" != Darwin ]]; then
  echo "skip scandeps/testdata/generate_test_hmap.sh: " \
       "need to run on Darwin" 1>&2
  exit
fi
mkdir -p /private/tmp/siso-scandeps-test/out/siso
(cd /private/tmp/siso-scandeps-test/out/siso && \
 repodir="${this_dir}/../../subcmd/ninja/testdata/TestBuild_Hmap"
 python3 "${repodir}/build/config/ios/write_framework_hmap.py" \
    test.hmap \
    Foo.framework \
    ../../ios/fooFramework/Foo.h \
    ../../ios/fooFramework/Bar.h \
    ../../ios/fooFramework/Baz.h)
cp /private/tmp/siso-scandeps-test/out/siso/test.hmap testdata/test.hmap

# expected
#  Foo/Foo.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Foo.h
#  Foo/Bar.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Bar.h
#  Foo/Baz.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Baz.h
#  Foo.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Foo.h
#  Bar.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Bar.h
#  Baz.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Baz.h
