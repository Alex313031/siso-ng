// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cred

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

type luciAuthTokenSource struct {
	luciAuthPath string

	// contextArgs is args for `luci-auth context` (including "context")
	// e.g. []string{"context", "--act-as-service-account" "xxx"}.
	contextArgs []string

	mu sync.Mutex
	// args for "luci-auth token"
	// e.g. []string{"--scope-context"}.
	// if empty, use siso's default scopes.
	args []string

	once  sync.Once
	email string
}

func (ts *luciAuthTokenSource) String() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.args) == 0 {
		return "luci-auth-cloud-platform"
	}
	if slices.Equal(ts.args, []string{"--scopes-context"}) {
		return "luci-auth-context"
	}
	return "luci-auth-custom"
}

func (ts *luciAuthTokenSource) flags() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.args) > 0 {
		return ts.args
	}
	return []string{
		"--scopes",
		strings.Join([]string{
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/userinfo.email",
		}, " "),
	}
}

func (ts *luciAuthTokenSource) fallback() bool {
	if len(ts.contextArgs) > 0 {
		return false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.args) > 0 {
		return false
	}
	ts.args = []string{"--scopes-context"}
	return true
}

type luciAuthTokenResponse struct {
	Token  string `json:"token"`
	Expiry int64  `json:"expiry"`
}

func (ts *luciAuthTokenSource) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		args := []string{
			ts.luciAuthPath,
			"token",
			"--json-output",
			"-",
		}
		args = append(args, ts.flags()...)
		if len(ts.contextArgs) > 0 {
			// run as `luci-auth context [context args..] -- luci-auth token [token args...]`
			args = slices.Concat([]string{ts.luciAuthPath}, ts.contextArgs, []string{"--"}, args)
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			if ts.fallback() {
				continue // retry
			}
			if cmd.ProcessState.ExitCode() == 1 && bytes.Contains(stderr.Bytes(), []byte("Not logged in.")) {
				return nil, errors.New(stderr.String())
			}
			return nil, fmt.Errorf("failed to get token source: %w\nstdout:%s stderr:%s", err, stdout.String(), stderr.String())
		}
		var resp luciAuthTokenResponse
		err = json.Unmarshal(stdout.Bytes(), &resp)
		if err != nil {
			return nil, fmt.Errorf("failed to parse response %q: %w", stdout.Bytes(), err)
		}
		tok := &oauth2.Token{
			AccessToken: resp.Token,
			Expiry:      time.Unix(resp.Expiry, 0),
		}
		tok = tok.WithExtra(map[string]any{
			"x-token-source": ts.String(),
			"x-token-email":  ts.Email(),
		})
		return tok, nil
	}
}

type luciAuthInfoResponse struct {
	Email string `json:"email"`
	// no need for "client_id", "scopes"
}

func (ts *luciAuthTokenSource) Email() string {
	ts.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if ts.email != "" {
			return
		}
		args := []string{
			ts.luciAuthPath,
			"info",
			"--json-output",
			"-",
		}
		args = append(args, ts.flags()...)
		if len(ts.contextArgs) > 0 {
			args = slices.Concat([]string{ts.luciAuthPath}, ts.contextArgs, args)
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		out, err := cmd.Output()
		if err != nil {
			return
		}
		var resp luciAuthInfoResponse
		err = json.Unmarshal(out, &resp)
		if err != nil {
			return
		}
		ts.email = resp.Email
	})
	return ts.email
}
