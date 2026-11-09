// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package cred

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"go.chromium.org/build/siso/ui"
)

// DefaultCredentialHelper returns default credential helper's path.
func DefaultCredentialHelper() string {
	if h, ok := os.LookupEnv("SISO_CREDENTIAL_HELPER"); ok {
		return h
	}
	if os.Getenv("RBE_tls_client_auth_cert") != "" && os.Getenv("RBE_tls_client_auth_key") != "" {
		return "mTLS"
	}
	if checkIfGoogleCredHelperExists() {
		return googleCredHelper
	}
	path, err := exec.LookPath("luci-auth")
	if err == nil {
		return path
	}
	return "google-application-default"
}

func checkIfGoogleCredHelperExists() bool {
	// googleCredHelper depends on stubby.
	_, err := exec.LookPath("stubby")
	if err != nil {
		return false
	}
	// Make sure it's not a laptop. gLaptop should fall back to luci-auth below.
	// See also go/glinux-roles.
	dist, err := os.ReadFile("/etc/lsb-release")
	if err != nil {
		ui.Default.Warningf("WARNING: Failed to read /etc/lsb-release. Assuming this is not a laptop. err: %s", err)
	}
	if bytes.Contains(dist, []byte("GOOGLE_ROLE=laptop")) {
		// stubby is not usable on gLaptop
		return false
	}
	// workaround for b/360055934
	// expect loas_check installed on glinux.
	_, err = exec.LookPath("loas_check")
	if err != nil {
		return false
	}
	out, err := exec.Command("loas_check", "--show_restrictions").Output()
	if err != nil {
		ui.Default.Errorf(`loas_check failed. need to run "gcert"
or set SISO_CREDENTIAL_HELPER explicitly.
`)
		os.Exit(1)
	}
	if bytes.Contains(out, []byte("destination_restriction")) {
		// destination_restriction block exists.
		// check if permission for srcfs/piper was granted.
		if !bytes.Contains(out, []byte(`"srcfs"`)) && !bytes.Contains(out, []byte(`"piper"`)) && !bytes.Contains(out, []byte(`"piper-srcfs-dispatcher"`)) {
			ui.Default.Errorf(`restricted user without srcfs permissions.
need RPC access: http//go/request-rpc
or set SISO_CREDENTIAL_HELPER explicitly.
`)
			os.Exit(1)
		}
	}
	// no destination_restriction, or have /google/src permission.
	ch := make(chan bool, 3)
	for i := range 3 {
		go func() {
			if fi, err := os.Stat(googleCredHelper); (err == nil && fi.Mode()&0111 != 0) || errors.Is(err, syscall.ENOKEY) {
				ch <- true
			}
			ch <- false
		}()
		select {
		case ok := <-ch:
			return ok
		case <-time.After(5 * time.Second):
			if i == 0 {
				ui.Default.Warningf("WARNING: Accessing /google/src takes longer than expected. Retrying for 10 more seconds...\n")
			}
		}
	}
	ui.Default.Errorf(`ERROR: Timeout while accessing /google/src.
Run "diagnose_me" or you would need RPC access: http://go/request-rpc
or set SISO_CREDENTIAL_HELPER explicitly.
`)
	os.Exit(1)
	return false
}

func credHelperErr(fname string, err error) error {
	if fname == googleCredHelper && errors.Is(err, syscall.ENOKEY) {
		return fmt.Errorf("need to run `gcert`: %w", syscall.ENOKEY)
	}
	return err
}
