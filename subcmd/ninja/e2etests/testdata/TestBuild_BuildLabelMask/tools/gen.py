# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import sys
import os
import shutil

os.makedirs(os.path.dirname(sys.argv[2]), exist_ok=True)
shutil.copyfile(sys.argv[1], sys.argv[2])
