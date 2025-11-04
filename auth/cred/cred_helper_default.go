// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !linux

package cred

import (
	"os"
	"os/exec"
)

// DefaultCredentialHelper returns default credential helper's path.
func DefaultCredentialHelper() string {
	if os.Getenv("RBE_tls_client_auth_cert") != "" && os.Getenv("RBE_tls_client_auth_key") != "" {
		return "mTLS"
	}
	path, err := exec.LookPath("luci-auth")
	if err == nil {
		return path
	}
	path, err = exec.LookPath("gcloud")
	if err == nil {
		return path
	}
	return ""
}

func credHelperErr(fname string, err error) error {
	return err
}
