// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"go.chromium.org/build/siso/hashfs"
)

// tempDir returns real path of temp dir.
// mac uses /tmp -> private/tmp symlink, so TempDir may contains
// symlink in the path, which confuses hashfs, so use EvalSymlinks
// to make it real path.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestScanDeps(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"base/base.h": `
#include <atomic>

#include "base/extra.h"
#include "base/allocator/allocator_extension.h"
`,
		"base/extra.h": `
#include <map>
#include <string>

#include "base/base_export.h"
`,
		"base/base_export.h": `
`,
		"base/allocator/allocator_extension.h": `
#include "base/base_export.h"
`,
		"apps/apps.h": `
#include <string>
#include "base/base.h"
`,
		"apps/apps.cc": `
#include <unistd.h>

#include <string>
#include "apps/apps.h"
#include "glog/logging.h"
`,
		"third_party/glog/src/glog/logging.h": `
#include <string>
#include <vector>
#include "glog/export.h"
`,
		"third_party/glog/src/glog/export.h": `
`,
		"build/third_party/libc++/trunk/include/__config": "",
		"build/third_party/libc++/trunk/include/atomic":   "",
		"build/third_party/libc++/trunk/include/string":   "",
		"build/third_party/libc++/trunk/include/vector":   "",
		"build/third_party/libc++/trunk/__config_site":    "",
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	inputDeps := map[string][]string{
		"build/linux/debian_bullseye_amd64-sysroot:headers": {
			"build/linux/debian_bullseye_amd64-sysroot/usr/include/unistd.h",
		},
		"build/third_party/libc++/trunk/include:headers": {
			"build/third_party/libc++/trunk/include/__config",
			"build/third_party/libc++/trunk/include/atomic",
			"build/third_party/libc++/trunk/include/string",
			"build/third_party/libc++/trunk/include/vector",
		},
		"build/third_party/libc++:headers": {
			"build/third_party/libc++/trunk/__config_site",
			"build/third_party/libc++/trunk/include:headers",
		},
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)

	req := Request{
		Sources: []string{
			"apps/apps.cc",
		},
		Dirs: []string{
			"",
			"third_party/glog/src",
			"build/third_party/libc++",
			"build/third_party/libc++/trunk/include",
		},
		Sysroots: []string{
			"build/linux/debian_bullseye_amd64-sysroot",
		},
	}

	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"apps",
		"apps/apps.cc",
		"apps/apps.h",
		"base",
		"base/allocator",
		"base/allocator/allocator_extension.h",
		"base/base.h",
		"base/base_export.h",
		"base/extra.h",
		"third_party/glog/src",
		"third_party/glog/src/glog",
		"third_party/glog/src/glog/export.h",
		"third_party/glog/src/glog/logging.h",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_SelfIncludeInCommentAndMacroInclude(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external/vma/vk_mem_alloc.h": `

#ifndef AMD_VULKAN_MEMORY_ALLOCATOR_H
#define AMD_VULKAN_MEMORY_ALLOCATOR_H

/*
    #include "vk_mem_alloc.h"
*/
#if !defined(VMA_CONFIGURATION_USER_INCLUDES_H)
    #include <mutex>
#else
    #include VMA_CONFIGURATION_USER_INCLUDES_H
#endif

#endif
`,
		"apps/apps.cc": `
#include "vma/vk_mem_alloc.h"
`,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	inputDeps := map[string][]string{}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)

	req := Request{
		Sources: []string{
			"apps/apps.cc",
		},
		Dirs: []string{
			"",
			"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external",
		},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"apps",
		"apps/apps.cc",
		"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external",
		"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external/vma",
		"third_party/vulkan-deps/vulkan-validation-layers/src/layers/external/vma/vk_mem_alloc.h",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_IncludeByDifferentMacroValue(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"third_party/harfbuzz-ng/src/src/hb-subset.cc": `
#include "hb-ot-post-table.hh"
#include "hb-ot-cff1-table.hh"
`,
		"third_party/harfbuzz-ng/src/src/hb-ot-post-table.hh": `
#ifndef HB_OT_POST_TABLE_HH
#define HB_OT_POST_TABLE_HH

#define HB_STRING_ARRAY_NAME format1_names
#define HB_STRING_ARRAY_LIST "hb-ot-post-macroman.hh"
#include "hb-string-array.hh"
#undef HB_STRING_ARRAY_LIST
#undef HB_STRING_ARRAY_NAME

#endif
`,
		"third_party/harfbuzz-ng/src/src/hb-ot-cff1-table.hh": `
#ifndef HB_OT_CFF1_TABLE_HH
#define HB_OT_CFF1_TABLE_HH

#define HB_STRING_ARRAY_NAME cff1_std_strings
#define HB_STRING_ARRAY_LIST "hb-ot-cff1-std-str.hh"
#include "hb-string-array.hh"
#undef HB_STRING_ARRAY_LIST
#undef HB_STRING_ARRAY_NAME

#endif
`,
		"third_party/harfbuzz-ng/src/src/hb-string-array.hh": `
#ifndef HB_STRING_ARRAY_HH
#if 0 /* Make checks happy. */
#define HB_STRING_ARRAY_HH
#endif

#include HB_STRING_ARRAY_LIST

#endif
`,
		"third_party/harfbuzz-ng/src/src/hb-ot-post-macroman.hh": "",
		"third_party/harfbuzz-ng/src/src/hb-ot-cff1-std-str.hh":  "",
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	inputDeps := map[string][]string{}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)

	req := Request{
		Sources: []string{
			"third_party/harfbuzz-ng/src/src/hb-subset.cc",
		},
		Dirs: []string{
			"",
			"third_party/harfbuzz-ng/src/src",
		},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"third_party/harfbuzz-ng/src/src",
		"third_party/harfbuzz-ng/src/src/hb-subset.cc",
		"third_party/harfbuzz-ng/src/src/hb-ot-post-table.hh",
		"third_party/harfbuzz-ng/src/src/hb-ot-cff1-table.hh",
		"third_party/harfbuzz-ng/src/src/hb-string-array.hh",
		"third_party/harfbuzz-ng/src/src/hb-ot-post-macroman.hh",
		"third_party/harfbuzz-ng/src/src/hb-ot-cff1-std-str.hh",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_Framework(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"app/app.mm": `
#import <Foo/Bar.h>
`,
		"out/siso/Foo.framework/Versions/A/Headers/Bar.h": `
// Bar.h
#import "Baz.h"
`,
		"out/siso/Foo.framework/Versions/A/Headers/Baz.h": `
// Baz.h
`,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := os.Symlink("Versions/Current/Headers", filepath.Join(dir, "out/siso/Foo.framework/Headers"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("A", filepath.Join(dir, "out/siso/Foo.framework/Versions/Current"))
	if err != nil {
		t.Fatal(err)
	}
	inputDeps := map[string][]string{}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)

	req := Request{
		Sources: []string{
			"app/app.mm",
		},
		Dirs: []string{},
		Frameworks: []string{
			"out/siso",
		},
		Sysroots: []string{},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	// symlink to dir (Foo.framework/Headers) and real dir
	// for the symlink (Foo.framework/Versions/Current/Headers).
	want := []string{
		"app",
		"app/app.mm",
		"out/siso",
		"out/siso/Foo.framework/Headers",
		"out/siso/Foo.framework/Headers/Bar.h",
		"out/siso/Foo.framework/Headers/Baz.h",
		"out/siso/Foo.framework/Versions/A/Headers",
		"out/siso/Foo.framework/Versions/Current",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_AbsPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("need to check on darwin only for swift generated header, and fails on windows in handling abs path?")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"app/app.mm": `
#include "popup_swift.h"
`,
		"ios/popup_swift_bridge.h": `
#include "ios/ios_string.h"
`,
		"ios/ios_string.h": `
// ios_string.h
`,
		"out/siso/gen/popup_swift.h": `
// generated by swiftc.py
` + fmt.Sprintf(`#import %q
`, filepath.Join(dir, "ios/popup_swift_bridge.h")),
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	inputDeps := map[string][]string{}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)

	req := Request{
		Sources: []string{
			"app/app.mm",
		},
		Dirs: []string{
			"",
			"out/siso/gen",
		},
		Sysroots: []string{},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		".",
		"app",
		"app/app.mm",
		"ios",
		"ios/ios_string.h",
		"ios/popup_swift_bridge.h",
		"out/siso/gen",
		"out/siso/gen/popup_swift.h",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_SymlinkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("no symlink on windows")
		return
	}

	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"x/logging.cc": `
#include "base/logging.h"
`,
		"src/base/logging.h": `
#ifndef BASE_LOGGING_H_
#define BASE_LOGGING_H_

#include <stddef.h>

#endif
`,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := os.Symlink("../x", filepath.Join(dir, "src/symlink_to_code"))
	if err != nil {
		t.Fatal(err)
	}

	inputDeps := map[string][]string{
		"build/linux/debian_bullseye_amd64-sysroot:headers": {
			"build/linux/debian_bullseye_amd64-sysroot/usr/include/unistd.h",
		},
		"build/third_party/libc++/trunk/include:headers": {
			"build/third_party/libc++/trunk/include/__config",
			"build/third_party/libc++/trunk/include/atomic",
			"build/third_party/libc++/trunk/include/string",
			"build/third_party/libc++/trunk/include/vector",
		},
		"build/third_party/libc++:headers": {
			"build/third_party/libc++/trunk/__config_site",
			"build/third_party/libc++/trunk/include:headers",
		},
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)

	req := Request{
		Sources: []string{
			"symlink_to_code/logging.cc",
		},
		Dirs: []string{
			"",
			"build/third_party/libc++",
			"build/third_party/libc++/trunk/include",
		},
		Sysroots: []string{
			"build/linux/debian_bullseye_amd64-sysroot",
		},
	}

	got, err := scanDeps.Scan(ctx, filepath.Join(dir, "src"), req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	// symlink_to_code is symlink but to out of exec root.
	// hashfs Entries will resolve it as real one (i.e. directory)
	// when it goes out of exec root.
	want := []string{
		"base",
		"base/logging.h",
		"symlink_to_code",
		"symlink_to_code/logging.cc",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_SymlinkIntermediateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("no symlink on windows")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"src/source.cc": `
#include <android/log.h>
`,
		"include/android/log.h": ``,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := os.MkdirAll(filepath.Join(dir, "include_vndk"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("../include/android", filepath.Join(dir, "include_vndk/android"))
	if err != nil {
		t.Fatal(err)
	}
	inputDeps := map[string][]string{
		"prebuilts/clang/host/linux-x86/clang-r563880:headers": {
			"prebuilts/clang/host/linux-x86/clang-r563880/bin/clang",
		},
		"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot:headers": {
			"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot/usr/include/unistd.h",
		},
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)
	req := Request{
		Sources: []string{
			"src/source.cc",
		},
		Dirs: []string{
			"include_vndk",
		},
		Sysroots: []string{
			"prebuilts/clang/host/linux-x86/clang-r563880:headers",
			"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot:headers",
		},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"include/android",
		"include_vndk",
		"include_vndk/android",
		"include_vndk/android/log.h",
		"src",
		"src/source.cc",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_SymlinkDirSymlinkIntermediateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("no symlink on windows")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"src/source.cc": `
#include <utils/RWLock.h>
`,
		"system/core/libutils/include/utils/RWLock.h": `
#include <utils/Errors.h>
`,
		"system/core/libutils/binder/include/utils/Errors.h": ``,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := os.MkdirAll(filepath.Join(dir, "system/core/include"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("../libutils/include/utils/", filepath.Join(dir, "system/core/include/utils"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("../../binder/include/utils/Errors.h", filepath.Join(dir, "system/core/libutils/include/utils/Errors.h"))
	if err != nil {
		t.Fatal(err)
	}
	inputDeps := map[string][]string{
		"prebuilts/clang/host/linux-x86/clang-r563880:headers": {
			"prebuilts/clang/host/linux-x86/clang-r563880/bin/clang",
		},
		"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot:headers": {
			"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot/usr/include/unistd.h",
		},
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)
	req := Request{
		Sources: []string{
			"src/source.cc",
		},
		Dirs: []string{
			"system/core/include",
		},
		Sysroots: []string{
			"prebuilts/clang/host/linux-x86/clang-r563880:headers",
			"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot:headers",
		},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"src",
		"src/source.cc",
		"system/core/include",
		"system/core/include/utils",
		"system/core/include/utils/Errors.h",
		"system/core/include/utils/RWLock.h",
		"system/core/libutils/binder/include/utils/Errors.h",
		"system/core/libutils/include/utils",
		"system/core/libutils/include/utils/Errors.h",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}
}

func TestScanDeps_SymlinkFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("no symlink on windows")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	for fname, content := range map[string]string{
		"src/source.cc": `
#include <log/log_id.h>
`,
		"include/log/log_id.h": ``,
	} {
		fname := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := os.MkdirAll(filepath.Join(dir, "include_vndk/log"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("../../include/log/log_id.h", filepath.Join(dir, "include_vndk/log/log_id.h"))
	if err != nil {
		t.Fatal(err)
	}
	inputDeps := map[string][]string{
		"prebuilts/clang/host/linux-x86/clang-r563880:headers": {
			"prebuilts/clang/host/linux-x86/clang-r563880/bin/clang",
		},
		"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot:headers": {
			"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot/usr/include/unistd.h",
		},
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	scanDeps := New(hashFS, inputDeps, nil)
	req := Request{
		Sources: []string{
			"src/source.cc",
		},
		Dirs: []string{
			"include_vndk",
		},
		Sysroots: []string{
			"prebuilts/clang/host/linux-x86/clang-r563880:headers",
			"prebuilts/gcc/linux-x86/host/x86_64-linux-glibc2.17-4.8/sysroot:headers",
		},
	}
	got, err := scanDeps.Scan(ctx, dir, req)
	if err != nil {
		t.Errorf("scandeps()=%v, %v; want nil err", got, err)
	}

	want := []string{
		"include/log/log_id.h",
		"include_vndk",
		"include_vndk/log",
		"include_vndk/log/log_id.h",
		"src",
		"src/source.cc",
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("scandeps diff -want +got:\n%s", diff)
	}

}
