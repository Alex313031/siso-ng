// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cred

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type gcloudTokenSource struct{}

func (gcloudTokenSource) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get token %s: %w", string(out), err)
	}
	token := strings.TrimSpace(string(out))

	// TODO: Propagate context when https://github.com/golang/oauth2/issues/262 is fixed.
	return fromTokenString(context.TODO(), "gcloud", token)
}
