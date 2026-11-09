// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package scandeps provides forged C/C++ dependency scanner.
//
// Scandeps is used to construct inputs for remote execution,
// since typical cc/cxx actions don't specify required inputs
// for remote execution.
// It is not used to check whether the action needs to
// re-execute or not. Such dependency information should be
// provided by the build graph or a depfile.
//
// Compared with Goma's input processor, scandeps only supports simple
// form of C preprocessor directives and uses precomputed subtree
// for sysroots or complicated include dirs.
//
// Scandeps only checks the following forms of #include
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
// Since scandeps doesn't process `#if` or `#ifdef`, it expands all possible
// values of macros for `#include FOO_H`.  Using extra inputs is
// not problem, but may have potential cache miss issues, since
// there is discrepancy between simple scandeps vs clang's *.d outputs.
// TODO(b/283341125): fix cache miss issue.
//
// Scandeps doesn't allow comments nor multiline (\ at the end of line)
// for the directives.
//
// Also scandeps uses input_deps's label for sysroots etc.
// if include dir or sysroot dir has label with `:headers`,
// it adds files of the input_deps instead of scanning files
// in the dir.  Rather using minimum sets of include dirs,
// it may use more files, but can use precomputed merkletree
// to improve performance in digest calculation for action inputs.
// It won't scan inside sysroot or include dir if it uses
// `:headers` input_deps for the compilation unit.
// Missing standard library headers or sysroot files in remote execution
// indicate missing or misconfigured input_deps/filegroups,
// not a failure of scandeps scanning.
package scandeps
