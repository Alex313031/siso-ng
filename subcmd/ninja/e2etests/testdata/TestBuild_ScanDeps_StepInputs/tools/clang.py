# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

with open("obj/base/base.o", "w"):
  pass
with open("obj/base/base.o.d", "w") as w:
  w.write("obj/base/base.o: ../../base/base.cc ../../base/base.h\n")
