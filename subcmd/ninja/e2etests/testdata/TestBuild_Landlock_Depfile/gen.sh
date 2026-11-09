#!/bin/sh
# Copyright 2026 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

mkdir -p obj
echo "content" > obj/foo.o
echo "obj/foo.o: ../../foo.cc ../../declared.h ../../undeclared.h" > obj/foo.o.d
