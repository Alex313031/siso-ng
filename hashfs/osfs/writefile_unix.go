// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package osfs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

var executableOnce = sync.OnceValues(os.Executable)

func writeFile(name string, data []byte, perm os.FileMode) error {
	if perm&0100 == 0 {
		return os.WriteFile(name, data, perm)
	}
	// workaround for https://github.com/golang/go/issues/22315
	//
	// ETXTBSY race when creating executable and running the executable
	// from the same process, because when starting a command (not
	// running the executable), fork inherits file descriptor of creating
	// executable, which will be closed when exec by O_CLOEXEC, but
	// ETXTBSY if running executable starts earlier before the exec.
	//
	// Goma used subprocess server, and gomax used localexec-server,
	// so that fork&exec from the process that didn't create executables.
	// but it spawns new sub process and it involves some complexity
	// (i.e. we'll use unix domain socket to communicate, but we can't
	// create unix domain socket in Cog workspace)
	//
	// It will run a new process to create executable file, so that
	// any build steps won't inherit file descriptor to write executable
	// file.  The file descriptor should have been closed when this process
	// finished.
	executable, err := executableOnce()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "install-helper", "-m", fmt.Sprintf("0%o", perm), "-o", name)
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}

func openForWrite(name string, perm os.FileMode) (io.WriteCloser, error) {
	if perm&0100 == 0 {
		return os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	}
	// need to use install-helper (other process than siso)
	// to write executable.
	// see comment in writeFile above.
	executable, err := executableOnce()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, "install-helper", "-m", fmt.Sprintf("0%o", perm), "-o", name)
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	err = cmd.Start()
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	return writer{cmd: cmd, w: w}, err
}

type writer struct {
	cmd *exec.Cmd
	w   io.WriteCloser
}

func (w writer) Write(data []byte) (int, error) {
	return w.w.Write(data)
}

func (w writer) Close() error {
	err := w.w.Close()
	cerr := w.cmd.Wait()
	if err == nil {
		err = cerr
	}
	return err
}
