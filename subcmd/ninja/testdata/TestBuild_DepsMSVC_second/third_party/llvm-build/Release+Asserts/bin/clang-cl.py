# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

print("Note: including file: ../../base/foo.h")
print("Note: including file:   ../../other/other.h")
with open("foo.o", "w") as f:
  f.write("")
