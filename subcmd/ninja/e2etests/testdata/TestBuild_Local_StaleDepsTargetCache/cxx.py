# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

import pathlib
import sys

src = pathlib.Path(sys.argv[1])
out = pathlib.Path(sys.argv[2])
content = src.read_text()
deps = [sys.argv[1]]
if "include_gen" in content:
  gen_h = pathlib.Path("gen/foo.h")
  content += gen_h.read_text()
  deps.append("gen/foo.h")
out.write_text(content)
pathlib.Path(str(out) + ".d").write_text(f"{out}: {' '.join(deps)}\n")
