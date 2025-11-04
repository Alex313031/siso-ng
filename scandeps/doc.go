// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package scandeps provides forged C/C++ dependency scanner.
// Compared with Goma's input processor, it only supports simple
// form of C preprocessor directives and uses precomputed subtree
// for sysroots or complicated include dirs.
//
// It only checks the following forms of #include
//
//	#include "foo.h"
//	#include <foo.h>
//	#include FOO_H
//
// to support last case, it also checks the following forms of #define
//
//	#define FOO_H "foo.h"
//	#define FOO_H <foo.h>
//	#define FOO_H OTHER_FOO_H
//
// Since it doesn't process `#if` or `#ifdef`, it expands all possible
// values of macros for `#include FOO_H`.  Using extra inputs is
// not problem, but may have potential cache miss issues, since
// there is discrepancy between simple scandeps vs clang's *.d outputs.
// TODO(b/283341125): fix cache miss issue.
//
// It doesn't allow comments nor multiline (\ at the end of line)
// for the directives.
//
// Also it uses input_deps's label for sysroots etc.
// if include dir or sysroot dir has label with `:headers`,
// it adds files of the input_deps instead of scanning files
// in the dir.  Rather using minimum sets of include dirs,
// it may use more files, but can use precomputed merkletree
// to improve performance in digest calculation for action inputs.
package scandeps
