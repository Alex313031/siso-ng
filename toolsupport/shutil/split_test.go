// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package shutil

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		cmdline string
		want    []string
	}{
		{
			cmdline: `../../third_party/llvm-build/Release+Asserts/bin/clang++ -MD -MF obj/third_party/abseil/abseil/ostringstream.o.d -D_FORTIFY_SOURCE=2 -DCR_CLANG_REVISION=\"llvmorg-13-init-14086-ge1b8fde1-1\" -DNDEBUG -D_LIBCPP_ENABLE_NODISCARD -D_LIBCPP_HAS_NO_VENDOR_AVAILABILITY_ANNOTATIONS -DNDEBUG -DENABLE_LZMA -DHAVE_COUNTERZ=1 -I../.. -Igen -I../../third_party/abseil/src -fstack-protector-all -fPIE -g -pthread -fPIC -pipe -m64 -march=x86-64 --sysroot=../../third_party/chromium_build/linux/debian_sid_amd64-sysroot -O2 -msse2 -fdata-sections -ffunction-sections -Wno-unused-result -Wno-format -Wno-misleading-indentation -Wno-implicit-int-float-conversion -std=c++14 -fno-rtti -nostdinc++ -isystem../../buildtools/third_party/libc++/trunk/include -isystem../../buildtools/third_party/libc++abi/trunk/include -fno-exceptions -c ../../third_party/abseil/src/absl/strings/internal/ostringstream.cc  -o obj/third_party/abseil/abseil/ostringstream.o`,
			want: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"-MD",
				"-MF",
				"obj/third_party/abseil/abseil/ostringstream.o.d",
				"-D_FORTIFY_SOURCE=2",
				`-DCR_CLANG_REVISION="llvmorg-13-init-14086-ge1b8fde1-1"`,
				"-DNDEBUG",
				"-D_LIBCPP_ENABLE_NODISCARD",
				"-D_LIBCPP_HAS_NO_VENDOR_AVAILABILITY_ANNOTATIONS",
				"-DNDEBUG",
				"-DENABLE_LZMA",
				"-DHAVE_COUNTERZ=1",
				"-I../..",
				"-Igen",
				"-I../../third_party/abseil/src",
				"-fstack-protector-all",
				"-fPIE",
				"-g",
				"-pthread",
				"-fPIC",
				"-pipe",
				"-m64",
				"-march=x86-64",
				"--sysroot=../../third_party/chromium_build/linux/debian_sid_amd64-sysroot",
				"-O2",
				"-msse2",
				"-fdata-sections",
				"-ffunction-sections",
				"-Wno-unused-result",
				"-Wno-format",
				"-Wno-misleading-indentation",
				"-Wno-implicit-int-float-conversion",
				"-std=c++14",
				"-fno-rtti",
				"-nostdinc++",
				"-isystem../../buildtools/third_party/libc++/trunk/include",
				"-isystem../../buildtools/third_party/libc++abi/trunk/include",
				"-fno-exceptions",
				"-c",
				"../../third_party/abseil/src/absl/strings/internal/ostringstream.cc",
				"-o",
				"obj/third_party/abseil/abseil/ostringstream.o",
			},
		},
		{
			cmdline: `python3 ../../build/toolchain/clang_code_coverage_wrapper.py --files-to-instrument=../../.code-coverage/files_to_instrument.txt --target-os=mac ../../third_party/llvm-build/Release+Asserts/bin/clang -MMD -MF 'clang_arm64_v8_x64/obj/third_party/xnnpack/amalgam_arch=armv8.2-a+i8mm+fp16/neoni8mm.o'.d -DDCHECK_ALWAYS_ON=1 -D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_EXTENSIVE -DCR_XCODE_VERSION=1500 -DCR_CLANG_REVISION=\"llvmorg-18-init-16072-gc4146121e940-5\" -c ../../third_party/xnnpack/src/src/amalgam/gen/neoni8mm.c -o 'clang_arm64_v8_x64/obj/third_party/xnnpack/amalgam_arch=armv8.2-a+i8mm+fp16/neoni8mm.o'`,
			want: []string{
				"python3",
				"../../build/toolchain/clang_code_coverage_wrapper.py",
				"--files-to-instrument=../../.code-coverage/files_to_instrument.txt",
				"--target-os=mac",
				"../../third_party/llvm-build/Release+Asserts/bin/clang",
				"-MMD",
				"-MF",
				"clang_arm64_v8_x64/obj/third_party/xnnpack/amalgam_arch=armv8.2-a+i8mm+fp16/neoni8mm.o.d",
				"-DDCHECK_ALWAYS_ON=1",
				"-D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_EXTENSIVE",
				"-DCR_XCODE_VERSION=1500",
				`-DCR_CLANG_REVISION="llvmorg-18-init-16072-gc4146121e940-5"`,
				"-c",
				"../../third_party/xnnpack/src/src/amalgam/gen/neoni8mm.c",
				"-o",
				"clang_arm64_v8_x64/obj/third_party/xnnpack/amalgam_arch=armv8.2-a+i8mm+fp16/neoni8mm.o",
			},
		},
		{
			cmdline: `/bin/bash -c ""`,
			want: []string{
				"/bin/bash",
				"-c",
				"",
			},
		},
		{
			cmdline: ` /bin/bash  -c  ""  `,
			want: []string{
				"/bin/bash",
				"-c",
				"",
			},
		},
		{
			cmdline: `/bin/bash -c "(rm -f out/fname ) && (cp \"frameworks/fname\" \"out/fname\" )"`,
			want: []string{
				"/bin/bash",
				"-c",
				`(rm -f out/fname ) && (cp "frameworks/fname" "out/fname" )`,
			},
		},
		{
			cmdline: "out/host/linux-x86/bin/mojom_types_downgrader\texternal/libchrome/mojo/public/mojom/base/time.mojom\t\t--out-dir out/soong/.intermediates/external/libchrome/libmojo_mojom_downgraded_files/gen/ipc",
			want: []string{
				"out/host/linux-x86/bin/mojom_types_downgrader",
				"external/libchrome/mojo/public/mojom/base/time.mojom",
				"--out-dir",
				"out/soong/.intermediates/external/libchrome/libmojo_mojom_downgraded_files/gen/ipc",
			},
		},
	} {
		args, err := Split(tc.cmdline)
		if err != nil {
			t.Errorf("Split(%q)=%q, %v; want nil error", tc.cmdline, args, err)
		}
		if diff := cmp.Diff(tc.want, args); diff != "" {
			t.Errorf("Split(%q); diff -want +got:\n%s", tc.cmdline, diff)
		}
	}
}

func TestSplit_Error(t *testing.T) {
	for _, cmdline := range []string{
		`ln -f ../../client/report_env.sh report_env.sh 2>/dev/null || (rm -rf report_env.sh && cp -af ../../client/report_env.sh report_env.sh)`,
		`/bin/bash -c "`,
		`/bin/bash -c "(rm -out/fname ) && (cp \`,
		`cp foo bar\`,
	} {
		args, err := Split(cmdline)
		if err == nil {
			t.Errorf("Split(%q)=%q, %v; want err", cmdline, args, err)
		}
	}
}
