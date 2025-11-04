// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package straceutil

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestScanStraceData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		inputs  []string
		outputs []string
	}{
		{
			name: "clang",
			data: []byte(clangTraceTestData),
			inputs: []string{
				"../../third_party/llvm-build/Release+Asserts/bin/clang++",
				"/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/../lib",
				"/etc/ld.so.cache",
				"/lib/x86_64-linux-gnu/libpthread.so.0",
				"/lib/x86_64-linux-gnu/librt.so.1",
				"/lib/x86_64-linux-gnu/libdl.so.2",
				"/lib/x86_64-linux-gnu/libm.so.6",
				"/lib/x86_64-linux-gnu/libz.so.1",
				"/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/../lib/libstdc++.so.6",
				"/lib/x86_64-linux-gnu/libgcc_s.so.1",
				"/lib/x86_64-linux-gnu/libc.so.6",
				"../../third_party/llvm-build/Release+Asserts/bin",
				"/etc/os-release",
				"/etc/lsb-release",
				"/etc/debian_version",
				"/path/to/src/goma/client/third_party",
				"/path/to/src/goma/client/third_party/llvm-build",
				"/path/to/src/goma/client/third_party/llvm-build/Release+Asserts",
				"/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin",
				"/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/clang++",
				"/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/clang",
				"../../third_party/llvm-build/Release+Asserts",
				"../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib64",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/crtbegin.o",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib/x86_64-linux-gnu",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib/../lib64",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/x86_64-linux-gnu",
				"../../base/path.cc",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu",
				"../..",
				"gen",
				"../../base",
				"../../third_party/abseil/src",
				"../../third_party/chromium_base",
				"../../third_party/config/glog/linux",
				"../../third_party/glog/src",
				"../../buildtools/third_party/libc++/trunk/include",
				"../../buildtools/third_party/libc++abi/trunk/include",
				"../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include",
				"../../base/path.h",
				"../../buildtools/third_party/libc++/trunk/include/initializer_list",
				"../../buildtools/third_party/libc++/trunk/include/__config",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/features.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/stdc-predef.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/sys",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/sys/cdefs.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/wordsize.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/long-double.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/gnu",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/gnu/stubs.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/gnu/stubs-64.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/pthread.h",
				"../../buildtools/third_party/libc++/trunk/include/cstddef",
				"../../buildtools/third_party/libc++/trunk/include/version",
				"../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/stddef.h",
				"../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/__stddef_max_align_t.h",
				"../../buildtools/third_party/libc++/trunk/include/__nullptr",
				"../../buildtools/third_party/libc++/trunk/include/string",
				"../../buildtools/third_party/libc++/trunk/include/string_view",
				"../../buildtools/third_party/libc++/trunk/include/__string",
				"../../buildtools/third_party/libc++/trunk/include/algorithm",
				"../../buildtools/third_party/libc++/trunk/include/type_traits",
				"../../buildtools/third_party/libc++/trunk/include/cstring",
				"../../buildtools/third_party/libc++/trunk/include/string.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/string.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/libc-header-start.h",
				"../../buildtools/third_party/libc++/trunk/include/stddef.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/locale_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__locale_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/strings.h",
				"../../buildtools/third_party/libc++/trunk/include/utility",
				"../../buildtools/third_party/libc++/trunk/include/__tuple",
				"../../buildtools/third_party/libc++/trunk/include/cstdint",
				"../../buildtools/third_party/libc++/trunk/include/stdint.h",
				"../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/stdint.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/stdint.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/timesize.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/typesizes.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/time64.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/wchar.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/stdint-intn.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/stdint-uintn.h",
				"../../buildtools/third_party/libc++/trunk/include/__debug",
				"../../buildtools/third_party/libc++/trunk/include/iosfwd",
				"../../buildtools/third_party/libc++/trunk/include/wchar.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/wchar.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/floatn.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/floatn-common.h",
				"../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/stdarg.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/wint_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/mbstate_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__mbstate_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__FILE.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/FILE.h",
				"../../buildtools/third_party/libc++/trunk/include/memory",
				"../../buildtools/third_party/libc++/trunk/include/__availability",
				"../../buildtools/third_party/libc++/trunk/include/typeinfo",
				"../../buildtools/third_party/libc++/trunk/include/exception",
				"../../buildtools/third_party/libc++/trunk/include/__memory",
				"../../buildtools/third_party/libc++/trunk/include/__memory/base.h",
				"../../buildtools/third_party/libc++/trunk/include/__undef_macros",
				"../../buildtools/third_party/libc++/trunk/include/cstdlib",
				"../../buildtools/third_party/libc++/trunk/include/stdlib.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/stdlib.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/waitflags.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/waitstatus.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/sys/types.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/clock_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/clockid_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/time_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/timer_t.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/endian.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/endian.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/endianness.h",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/byteswap.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/uintn-identity.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/sys/select.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/select.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/sigset_t.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__sigset_t.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_timeval.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_timespec.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/pthreadtypes.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/thread-shared-types.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/pthreadtypes-arch.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/struct_mutex.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/struct_rwlock.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/alloca.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/stdlib-float.h",

				"../../buildtools/third_party/libc++/trunk/include/new",

				"../../buildtools/third_party/libc++/trunk/include/limits",

				"../../buildtools/third_party/libc++/trunk/include/iterator",

				"../../buildtools/third_party/libc++/trunk/include/__functional_base",

				"../../buildtools/third_party/libc++/trunk/include/__memory/pointer_traits.h",

				"../../buildtools/third_party/libc++/trunk/include/tuple",

				"../../buildtools/third_party/libc++/trunk/include/stdexcept",

				"../../buildtools/third_party/libc++/trunk/include/__memory/allocator_traits.h",

				"../../buildtools/third_party/libc++/trunk/include/__memory/utilities.h",

				"../../buildtools/third_party/libc++/trunk/include/atomic",

				"../../buildtools/third_party/libc++/trunk/include/__threading_support",

				"../../buildtools/third_party/libc++/trunk/include/chrono",

				"../../buildtools/third_party/libc++/trunk/include/ctime",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/time.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/time.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/timex.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_tm.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_itimerspec.h",

				"../../buildtools/third_party/libc++/trunk/include/ratio",

				"../../buildtools/third_party/libc++/trunk/include/climits",

				"../../buildtools/third_party/libc++/trunk/include/limits.h",

				"../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/limits.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/limits.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/posix1_lim.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/local_lim.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/linux",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/linux/limits.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/posix2_lim.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/xopen_lim.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/uio_lim.h",

				"../../buildtools/third_party/libc++/trunk/include/errno.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/errno.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/errno.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/linux/errno.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/asm",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/asm/errno.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/asm-generic",
				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/asm-generic/errno.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/asm-generic/errno-base.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/error_t.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/sched.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/sched.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_sched_param.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/cpu-set.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/setjmp.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/semaphore.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/semaphore.h",

				"../../buildtools/third_party/libc++/trunk/include/functional",

				"../../buildtools/third_party/libc++/trunk/include/bit",

				"../../buildtools/third_party/libc++/trunk/include/__bits",

				"../../buildtools/third_party/libc++/trunk/include/cstdio",

				"../../buildtools/third_party/libc++/trunk/include/stdio.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/stdio.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__fpos_t.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__fpos64_t.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_FILE.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/cookie_io_functions_t.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/stdio_lim.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/sys_errlist.h",

				"../../buildtools/third_party/libc++/trunk/include/cwchar",

				"../../buildtools/third_party/libc++/trunk/include/cwctype",

				"../../buildtools/third_party/libc++/trunk/include/cctype",

				"../../buildtools/third_party/libc++/trunk/include/ctype.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/ctype.h",

				"../../buildtools/third_party/libc++/trunk/include/wctype.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/wctype.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/wctype-wchar.h",

				"../../third_party/abseil/src/absl/strings",
				"../../third_party/abseil/src/absl/strings/string_view.h",

				"../../third_party/abseil/src/absl/base",
				"../../third_party/abseil/src/absl/base/config.h",

				"../../third_party/abseil/src/absl/base/options.h",

				"../../third_party/abseil/src/absl/base/policy_checks.h",

				"../../buildtools/third_party/libc++/trunk/include/any",

				"../../buildtools/third_party/libc++/trunk/include/optional",

				"../../buildtools/third_party/libc++/trunk/include/variant",

				"../../buildtools/third_party/libc++/trunk/include/cassert",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/assert.h",

				"../../third_party/abseil/src/absl/base/internal",
				"../../third_party/abseil/src/absl/base/internal/throw_delegate.h",

				"../../third_party/abseil/src/absl/base/macros.h",

				"../../third_party/abseil/src/absl/base/attributes.h",

				"../../third_party/abseil/src/absl/base/optimization.h",

				"../../third_party/abseil/src/absl/base/port.h",

				"../../third_party/config/glog/linux/glog",
				"../../third_party/config/glog/linux/glog/logging.h",

				"../../buildtools/third_party/libc++/trunk/include/ostream",

				"../../buildtools/third_party/libc++/trunk/include/ios",

				"../../buildtools/third_party/libc++/trunk/include/__locale",

				"../../buildtools/third_party/libc++/trunk/include/mutex",

				"../../buildtools/third_party/libc++/trunk/include/__mutex_base",

				"../../buildtools/third_party/libc++/trunk/include/system_error",

				"../../buildtools/third_party/libc++/trunk/include/__errc",

				"../../buildtools/third_party/libc++/trunk/include/cerrno",

				"../../buildtools/third_party/libc++/trunk/include/locale.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/locale.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/locale.h",

				"../../buildtools/third_party/libc++/trunk/include/streambuf",

				"../../buildtools/third_party/libc++/trunk/include/locale",

				"../../buildtools/third_party/libc++/trunk/include/cstdarg",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/nl_types.h",

				"../../buildtools/third_party/libc++/trunk/include/__bsd_locale_fallbacks.h",

				"../../buildtools/third_party/libc++/trunk/include/bitset",

				"../../buildtools/third_party/libc++/trunk/include/__bit_reference",

				"../../buildtools/third_party/libc++/trunk/include/sstream",

				"../../buildtools/third_party/libc++/trunk/include/istream",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/unistd.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/posix_opt.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/environments.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/confname.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/getopt_posix.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/getopt_core.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/unistd_ext.h",

				"../../buildtools/third_party/libc++/trunk/include/vector",

				"../../buildtools/third_party/libc++/trunk/include/__split_buffer",

				"../../buildtools/third_party/libc++/trunk/include/inttypes.h",

				"../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/inttypes.h",

				"../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/inttypes.h",

				"../../third_party/config/glog/linux/glog/log_severity.h",

				"../../third_party/config/glog/linux/glog/vlog_is_on.h",
				"/path/to/src/goma/client/out/Default",
			},
			outputs: []string{
				"obj/base/base/path.o.d",
				"obj/base/base/path.o",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs, outputs := scanStraceData(t.Context(), tc.data)
			if diff := cmp.Diff(tc.inputs, inputs); diff != "" {
				t.Errorf("inputs: -want +got\n%s", diff)
			}
			if diff := cmp.Diff(tc.outputs, outputs); diff != "" {
				t.Errorf("outptus: -want +got\n%s", diff)
			}
		})
	}
}

const (
	clangTraceTestData = `3922674 execve("../../third_party/llvm-build/Release+Asserts/bin/clang++", ["../../third_party/llvm-build/Rel"..., "-MD", "-MF", "obj/base/base/path.o.d", "-DCR_CLANG_REVISION=\"llvmorg-13-"..., "-D_LIBCPP_ENABLE_NODISCARD", "-D_LIBCPP_HAS_NO_VENDOR_AVAILABI"..., "-D_DEBUG", "-D_GLIBCXX_DEBUG=1", "-DENABLE_LZMA", "-DHAVE_COUNTERZ=1", "-I../..", "-Igen", "-I../../base", "-I../../third_party/abseil/src", "-I../../third_party/chromium_bas"..., "-I../../third_party/config/glog/"..., "-I../../third_party/glog/src", "-fstack-protector-all", "-fPIE", "-g", "-pthread", "-fPIC", "-pipe", "-m64", "-march=x86-64", "--sysroot=../../third_party/chro"..., "-no-canonical-prefixes", "-Wall", "-Wextra", "-Wsign-compare", "-Wimplicit-fallthrough", ...], 0x7fff8c57e560 /* 40 vars */) = 0
3922674 readlink("/proc/self/exe", "/path/to/src/"..., 4096) = 92
3922674 stat("/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/../lib", {st_mode=S_IFDIR|0755, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "/etc/ld.so.cache", O_RDONLY|O_CLOEXEC) = 3
3922674 openat(AT_FDCWD, "/lib/x86_64-linux-gnu/libpthread.so.0", O_RDONLY|O_CLOEXEC) = 3
3922674 openat(AT_FDCWD, "/lib/x86_64-linux-gnu/librt.so.1", O_RDONLY|O_CLOEXEC) = 3
3922674 openat(AT_FDCWD, "/lib/x86_64-linux-gnu/libdl.so.2", O_RDONLY|O_CLOEXEC) = 3
3922674 openat(AT_FDCWD, "/lib/x86_64-linux-gnu/libm.so.6", O_RDONLY|O_CLOEXEC) = 3
3922674 openat(AT_FDCWD, "/lib/x86_64-linux-gnu/libz.so.1", O_RDONLY|O_CLOEXEC) = 3
3922674 openat(AT_FDCWD, "/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/../lib/libstdc++.so.6", O_RDONLY|O_CLOEXEC) = 3
3922674 openat(AT_FDCWD, "/lib/x86_64-linux-gnu/libgcc_s.so.1", O_RDONLY|O_CLOEXEC) = 3
3922674 openat(AT_FDCWD, "/lib/x86_64-linux-gnu/libc.so.6", O_RDONLY|O_CLOEXEC) = 3
3922674 access("../../third_party/llvm-build/Release+Asserts/bin/clang++", F_OK) = 0
3922674 access("../../third_party/llvm-build/Release+Asserts/bin", F_OK) = 0
3922674 openat(AT_FDCWD, "/etc/os-release", O_RDONLY|O_CLOEXEC) = 3
3922674 access("/proc/self/fd", R_OK)   = 0
3922674 readlink("/proc/self/fd/3", "/usr/lib/os-release", 4096) = 19
3922674 openat(AT_FDCWD, "/etc/lsb-release", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/etc/lsb-release", 4096) = 16
3922674 openat(AT_FDCWD, "/etc/debian_version", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/etc/debian_version", 4096) = 19
3922674 getcwd("/path/to/src/goma/client/out/Default", 4096) = 54
3922674 lstat("/path/to/src/goma/client/third_party", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 lstat("/path/to/src/goma/client/third_party/llvm-build", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 lstat("/path/to/src/goma/client/third_party/llvm-build/Release+Asserts", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 lstat("/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin", {st_mode=S_IFDIR|0755, st_size=4096, ...}) = 0
3922674 lstat("/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/clang++", {st_mode=S_IFLNK|0777, st_size=5, ...}) = 0
3922674 readlink("/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/clang++", "clang", 4095) = 5
3922674 lstat("/path/to/src/goma/client/third_party/llvm-build/Release+Asserts/bin/clang", {st_mode=S_IFREG|0755, st_size=67294256, ...}) = 0
3922674 stat("../../third_party/llvm-build/Release+Asserts", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("/path/to/src/goma/client/third_party/llvm-build/Release+Asserts", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/llvm-build/Release+Asserts", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("/path/to/src/goma/client/third_party/llvm-build/Release+Asserts", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0", {st_mode=S_IFDIR|0755, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib64", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu", O_RDONLY|O_NONBLOCK|O_CLOEXEC|O_DIRECTORY) = 3
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/crtbegin.o", {st_mode=S_IFREG|0640, st_size=2448, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu", O_RDONLY|O_NONBLOCK|O_CLOEXEC|O_DIRECTORY) = 3
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib/x86_64-linux-gnu", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib/../lib64", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib/x86_64-linux-gnu", {st_mode=S_IFDIR|0750, st_size=32768, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/lib", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/lib", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../base/path.cc", {st_mode=S_IFREG|0640, st_size=5106, ...}) = 0
3922674 stat(".", {st_mode=S_IFDIR|0700, st_size=20480, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat(".", {st_mode=S_IFDIR|0700, st_size=20480, ...}) = 0
3922674 stat("../..", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("gen", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../base", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/abseil/src", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_base", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/config/glog/linux", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/glog/src", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../buildtools/third_party/libc++/trunk/include", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../buildtools/third_party/libc++abi/trunk/include", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include", {st_mode=S_IFDIR|0755, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include", {st_mode=S_IFDIR|0750, st_size=12288, ...}) = 0
3922674 openat(AT_FDCWD, "../../base/path.cc", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 56
3922674 stat("obj/base/base/path.o", {st_mode=S_IFREG|0640, st_size=192920, ...}) = 0
3922674 access("obj/base/base/path.o", W_OK) = 0
3922674 openat(AT_FDCWD, "/dev/urandom", O_RDONLY) = 4
3922674 openat(AT_FDCWD, "obj/base/base/path-9bdd807d.o.tmp", O_RDWR|O_CREAT|O_EXCL|O_CLOEXEC, 0666) = 4
3922674 openat(AT_FDCWD, "../../base/path.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 55
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/initializer_list", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 104
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__config", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/features.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 124
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/stdc-predef.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 127
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/sys", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/sys/cdefs.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 142
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/wordsize.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 146
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/long-double.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 149
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/gnu", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/gnu/stubs.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 142
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/gnu/stubs-64.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 145
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/pthread.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 123
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cstddef", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/version", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/stddef.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 116
3922674 openat(AT_FDCWD, "../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/__stddef_max_align_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 130
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__nullptr", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 97
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/string", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/string_view", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 99
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__string", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/algorithm", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 97
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/type_traits", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 99
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cstring", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/string.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/string.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/libc-header-start.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 155
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/stddef.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/locale_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 152
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__locale_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 154
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/strings.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 123
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/utility", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__tuple", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cstdint", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/stdint.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/stdint.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 116
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/stdint.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 143
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/timesize.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 146
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/typesizes.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 147
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/time64.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 144
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/wchar.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 143
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/stdint-intn.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 149
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/stdint-uintn.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__debug", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/iosfwd", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/wchar.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/wchar.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 121
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/floatn.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 144
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/floatn-common.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 151
3922674 openat(AT_FDCWD, "../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/stdarg.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 116
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/wint_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/mbstate_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 153
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__mbstate_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 155
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__FILE.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/FILE.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 148
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/memory", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__availability", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 102
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/typeinfo", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/exception", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 97
3922674 stat("../../buildtools/third_party/libc++/trunk/include/__memory", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__memory/base.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 103
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__undef_macros", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 102
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cstdlib", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/stdlib.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/stdlib.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/waitflags.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 147
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/waitstatus.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 148
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/sys/types.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 142
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/clock_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 151
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/clockid_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 153
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/time_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/timer_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 151
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/endian.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/endian.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 144
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/endianness.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 148
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/byteswap.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 146
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/uintn-identity.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 152
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/sys/select.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 143
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/select.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 144
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/sigset_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 152
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__sigset_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 154
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_timeval.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 158
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_timespec.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 159
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/pthreadtypes.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/thread-shared-types.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 157
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/pthreadtypes-arch.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 155
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/struct_mutex.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/struct_rwlock.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 151
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/alloca.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/stdlib-float.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/new", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 91
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/limits", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/iterator", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__functional_base", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 105
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__memory/pointer_traits.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 113
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/tuple", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 93
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/stdexcept", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 97
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__memory/allocator_traits.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 115
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__memory/utilities.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 108
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/atomic", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__threading_support", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 107
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/chrono", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/ctime", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 93
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/time.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 120
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/time.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 142
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/timex.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 143
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_tm.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 153
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_itimerspec.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 161
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/ratio", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 93
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/climits", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/limits.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/limits.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 116
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/limits.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/posix1_lim.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 148
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/local_lim.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 147
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/linux", {st_mode=S_IFDIR|0750, st_size=20480, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/linux/limits.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 128
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/posix2_lim.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 148
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/xopen_lim.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 147
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/uio_lim.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 145
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/errno.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/errno.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 121
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/errno.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 143
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/linux/errno.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 127
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/asm", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/asm/errno.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 142
3922674 stat("../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/asm-generic", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/asm-generic/errno.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 133
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/asm-generic/errno-base.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 138
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/error_t.h", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 151
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/sched.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 121
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/sched.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 143
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_sched_param.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 162
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/cpu-set.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 145
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/setjmp.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 144
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/semaphore.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 125
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/semaphore.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 147
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/functional", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 98
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/bit", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 91
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__bits", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cstdio", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/stdio.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/stdio.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 121
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__fpos_t.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 152
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/__fpos64_t.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 154
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/struct_FILE.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 155
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/types/cookie_io_functions_t.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 165
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/stdio_lim.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 147
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/sys_errlist.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 149
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cwchar", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cwctype", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cctype", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/ctype.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/ctype.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 121
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/wctype.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/wctype.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/wctype-wchar.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 150
3922674 stat("../../third_party/abseil/src/absl/strings", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/strings/string_view.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 93
3922674 stat("../../third_party/abseil/src/absl/base", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/base/config.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 85
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/base/options.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 86
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/base/policy_checks.h", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 92
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/any", O_RDONLY|O_CLOEXEC) = 3
3922674 readlink("/proc/self/fd/3", "/path/to/src/"..., 4096) = 91
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/optional", O_RDONLY|O_CLOEXEC) = 5
3922674 readlink("/proc/self/fd/5", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/variant", O_RDONLY|O_CLOEXEC) = 6
3922674 readlink("/proc/self/fd/6", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cassert", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/assert.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 122
3922674 stat("../../third_party/abseil/src/absl/base/internal", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/base/internal/throw_delegate.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 102
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/base/macros.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 85
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/base/attributes.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 89
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/base/optimization.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 91
3922674 openat(AT_FDCWD, "../../third_party/abseil/src/absl/base/port.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 83
3922674 stat("../../third_party/config/glog/linux/glog", {st_mode=S_IFDIR|0750, st_size=4096, ...}) = 0
3922674 openat(AT_FDCWD, "../../third_party/config/glog/linux/glog/logging.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 88
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/ostream", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/ios", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 91
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__locale", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/mutex", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 93
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__mutex_base", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 100
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/system_error", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 100
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__errc", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cerrno", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/locale.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 96
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/locale.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/locale.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 144
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/streambuf", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 97
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/locale", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/cstdarg", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/nl_types.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 124
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__bsd_locale_fallbacks.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 112
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/bitset", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__bit_reference", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 103
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/sstream", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/istream", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 95
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/unistd.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 122
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/posix_opt.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 147
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/environments.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/confname.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 146
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/getopt_posix.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 150
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/getopt_core.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 149
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/x86_64-linux-gnu/bits/unistd_ext.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 148
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/vector", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 94
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/__split_buffer", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 102
3922674 openat(AT_FDCWD, "../../buildtools/third_party/libc++/trunk/include/inttypes.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 98
3922674 openat(AT_FDCWD, "../../third_party/llvm-build/Release+Asserts/lib/clang/13.0.0/include/inttypes.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 118
3922674 openat(AT_FDCWD, "../../third_party/chromium_build/linux/debian_sid_amd64-sysroot/usr/include/inttypes.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 124
3922674 openat(AT_FDCWD, "../../third_party/config/glog/linux/glog/log_severity.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 93
3922674 openat(AT_FDCWD, "../../third_party/config/glog/linux/glog/vlog_is_on.h", O_RDONLY|O_CLOEXEC) = 7
3922674 readlink("/proc/self/fd/7", "/path/to/src/"..., 4096) = 91
3922674 openat(AT_FDCWD, "obj/base/base/path.o.d", O_WRONLY|O_CREAT|O_TRUNC|O_CLOEXEC, 0666) = 7
3922674 rename("obj/base/base/path-9bdd807d.o.tmp", "obj/base/base/path.o") = 0
3922674 faccessat2(AT_FDCWD, "/path/to/src/goma/client/out/Default", F_OK, AT_EACCESS) = 0
3922674 newfstatat(AT_FDCWD, "/usr/bin/pyvenv.cfg", 0x7ffd069517e0, 0) = -1 ENOENT (No such file or directory)
3922674 readlink("/usr", 0x7ffd3d691cf0, 1023) = -1 EINVAL (Invalid argument)
3922674   newfstatat(AT_FDCWD, "/path/to/src/goma/client/out/Default", {st_mode=S_IFDIR|0755, st_size=450560, ...}, 0) = 0"
3922674 +++ exited with 0 +++

`
)
