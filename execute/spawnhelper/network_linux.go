// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package spawnhelper

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

func setNetworkPolicy(cmd *exec.Cmd, blockNetwork bool) error {
	if !blockNetwork {
		return nil
	}
	uid := os.Getuid()
	gid := os.Getgid()
	cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
		{ContainerID: uid, HostID: uid, Size: 1},
	}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
		{ContainerID: gid, HostID: gid, Size: 1},
	}
	cmd.SysProcAttr.AmbientCaps = []uintptr{
		12, // CAP_NET_ADMIN
	}
	return nil
}

func ifaceUp(ifname string) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_IP)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)

	type ifreq struct {
		Name  [16]byte
		Flags uint16
		Pad   [22]byte
	}
	var ifr ifreq
	copy(ifr.Name[:], ifname)

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x8913, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return fmt.Errorf("SIOCGIFFLAGS: %w", errno)
	}

	ifr.Flags |= 0x1 | 0x40

	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x8914, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return fmt.Errorf("SIOCSIFFLAGS: %w", errno)
	}
	return nil
}
