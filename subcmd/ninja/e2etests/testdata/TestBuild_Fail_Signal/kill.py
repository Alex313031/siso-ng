# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import os
import signal
import sys

print("standard output")
sys.stdout.flush()

print("standard error", file=sys.stderr)
sys.stderr.flush()

os.kill(os.getpid(), signal.SIGKILL)
