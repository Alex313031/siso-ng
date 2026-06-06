// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package cmdutil

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Split splits cmd.exe's cmdline.
func Split(cmdline string) ([]string, error) {
	var argc int32
	argsPtr, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	sysArgv, err := windows.CommandLineToArgv(argsPtr, &argc)
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(sysArgv)))
	args := make([]string, argc)
	for i, v := range (*sysArgv)[:argc] {
		// (*v) is [8192]uint16, but may be longer?
		s := unsafe.Slice(&v[0], len(cmdline))
		args[i] = windows.UTF16ToString(s)
	}
	runtime.KeepAlive(argsPtr)
	return args, nil
}
