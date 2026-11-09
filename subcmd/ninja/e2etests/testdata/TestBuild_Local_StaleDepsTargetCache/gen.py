# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import pathlib
import sys

data = pathlib.Path(sys.argv[1]).read_bytes()
pathlib.Path(sys.argv[2]).write_bytes(data)
