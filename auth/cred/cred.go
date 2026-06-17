// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package cred provides gRPC / API credentials to authenticate to network services.
package cred

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"

	"go.chromium.org/build/siso/o11y/clog"
)

// Cred holds credentials and derived values.
type Cred struct {
	// Type is credential type. e.g. "luci-auth", "gcloud", etc.
	Type string

	// Email is authenticated email.
	Email string

	perRPCCredentials credentials.PerRPCCredentials
	tokenSource       oauth2.TokenSource

	lazy *lazyCredWrapper
}

// Wait waits for background credential initialization.
func (c *Cred) Wait() error {
	if c.lazy != nil {
		c.lazy.wg.Wait()
		if c.lazy.err != nil {
			return c.lazy.err
		}
		c.Type = c.lazy.Type
		c.Email = c.lazy.Email
	}
	return nil
}

// Options is an options for credentials.
type Options struct {
	Type              string
	PerRPCCredentials credentials.PerRPCCredentials
	// TokenSource is used when PerRPCCredentials is not set.
	TokenSource oauth2.TokenSource

	login  func(context.Context) error
	logout func(context.Context) error
}

// AuthOpts returns the LUCI auth options that Siso uses.
func AuthOpts(credHelperPath string, args ...string) Options {
	switch credHelperPath {
	case "mTLS":
		return Options{Type: "mTLS"}
	case "google-application-default":
		return Options{
			Type: "google-application-default",
			login: func(ctx context.Context) error {
				cmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "login")
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				return cmd.Run()
			},
			logout: func(ctx context.Context) error {
				cmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "revoke")
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				return cmd.Run()
			},
		}
	case "":
		return Options{}
	}
	var perRPCCredentials credentials.PerRPCCredentials
	var tokenSource oauth2.TokenSource
	var login, logout func(context.Context) error
	base := filepath.Base(credHelperPath)
	authType := strings.TrimSuffix(base, filepath.Ext(base))
	switch authType {
	case "luci-auth":
		tokenSource = &luciAuthTokenSource{luciAuthPath: credHelperPath, contextArgs: args}
		login = func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, credHelperPath, "login", "--scopes", "https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/cloud-platform")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			if err != nil {
				return fmt.Errorf("%w\nIf you get 'This app is blocked', see https://chromium.googlesource.com/build/+/refs/heads/main/siso/docs/auth.md#this-app-is-blocked", err)
			}
			return nil
		}
		logout = func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, credHelperPath, "logout", "--scopes", "https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/cloud-platform")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
	case "gcloud":
		tokenSource = gcloudTokenSource{}
		login = func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, credHelperPath, "auth", "login", "--update-adc")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		logout = func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, credHelperPath, "auth", "revoke")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
	default:
		h := &credHelper{path: credHelperPath}
		perRPCCredentials = h
		tokenSource = &credHelperGoogle{h: h}
		if credHelperPath == googleCredHelper {
			login = func(ctx context.Context) error {
				fmt.Printf("running gcert\n")
				cmd := exec.CommandContext(ctx, "gcert")
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				return cmd.Run()
			}
			logout = func(ctx context.Context) error {
				fmt.Printf("running gcertdestroy\n")
				cmd := exec.CommandContext(ctx, "gcertdestroy")
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				return cmd.Run()
			}
		}
	}
	return Options{
		Type:              authType,
		PerRPCCredentials: perRPCCredentials,
		TokenSource:       tokenSource,
		login:             login,
		logout:            logout,
	}
}

func (o Options) Login(ctx context.Context) error {
	if o.login == nil {
		return fmt.Errorf("unsupported auth type for login")
	}
	return o.login(ctx)
}

func (o Options) Logout(ctx context.Context) error {
	if o.logout == nil {
		return fmt.Errorf("unsupported auth type for logout")
	}
	return o.logout(ctx)
}

// New creates a Cred using LUCI auth's default options.
// It ensures that the user is logged in and returns an error otherwise.
func New(ctx context.Context, uri string, opts Options) (Cred, error) {
	switch opts.Type {
	case "":
		return Cred{}, fmt.Errorf(`empty credential helper. need to set credential helper path, "luci-auth" or "google-application-default" in SISO_CREDENTIAL_HELPER`)
	case "google-application-default":
		adc, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return Cred{}, err
		}
		// TODO: context aware token source - https://github.com/golang/oauth2/issues/262
		errch := make(chan error, 1)
		go func() {
			_, err = adc.TokenSource.Token()
			errch <- err
		}()
		select {
		case <-ctx.Done():
			return Cred{}, context.Cause(ctx)
		case err = <-errch:
			if err != nil {
				return Cred{}, fmt.Errorf("google-application-default credential found, but invalid token\n on cloudtop, you need to run `gcloud application-default login`:\n %w", err)
			}
		}
	}
	if opts.TokenSource == nil {
		return Cred{Type: opts.Type}, nil
	}

	lc := &lazyCredWrapper{
		opts: opts,
		uri:  uri,
	}
	lc.wg.Go(func() { lc.init(ctx) })

	return Cred{
		Type:              opts.Type,
		Email:             "",
		perRPCCredentials: lc,
		tokenSource:       lc,
		lazy:              lc,
	}, nil
}

// grpcDialOptions returns grpc's dial options to use the credential.
func (c Cred) grpcDialOptions() []grpc.DialOption {
	perRPCCredentials := c.perRPCCredentials
	if perRPCCredentials == nil {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithPerRPCCredentials(perRPCCredentials),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
	}
}

// ClientOptions returns client options to use the credential.
func (c Cred) ClientOptions() []option.ClientOption {
	if c.Type == "google-application-default" {
		// Google Application Default Credentials will be used.
		return nil
	}
	// disable Google Application Default, and use PerRPCCredentials in dial option, or TokenSource
	// https://github.com/googleapis/google-api-go-client/issues/3149
	copts := []option.ClientOption{
		option.WithoutAuthentication(),
	}
	dopts := c.grpcDialOptions()
	if len(dopts) > 0 {
		for _, opt := range dopts {
			copts = append(copts, option.WithGRPCDialOption(opt))
		}
		return copts
	}
	if c.tokenSource == nil {
		return nil
	}
	return []option.ClientOption{
		option.WithTokenSource(c.tokenSource),
	}
}

// AuthorizationHeader returns authorization header for http request.
func (c Cred) AuthorizationHeader(ctx context.Context) string {
	tokenSource := c.tokenSource
	if tokenSource != nil && c.Type == "google-application-default" {
		adc, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			clog.Warningf(ctx, "failed to get adc: %v", err)
			return ""
		}
		tokenSource = adc.TokenSource
	}
	if tokenSource == nil {
		clog.Warningf(ctx, "no tokenSource %q", c.Type)
		return ""
	}
	token, err := tokenSource.Token()
	if err != nil {
		clog.Warningf(ctx, "failed to get token: %v", err)
		return ""
	}
	return fmt.Sprintf("Bearer %s", token.AccessToken)
}

type lazyCredWrapper struct {
	opts Options
	uri  string

	wg  sync.WaitGroup
	err error

	Type  string
	Email string

	perRPCCredentials credentials.PerRPCCredentials
	tokenSource       oauth2.TokenSource
}

func (c *lazyCredWrapper) init(ctx context.Context) {
	if c.opts.PerRPCCredentials != nil && c.uri != "" {
		_, err := c.opts.PerRPCCredentials.GetRequestMetadata(ctx, c.uri)
		if err == nil {
			t := "credential_helper"
			if ch, ok := c.opts.PerRPCCredentials.(*credHelper); ok {
				t = ch.path
			}
			c.Type = t
			c.perRPCCredentials = c.opts.PerRPCCredentials
			c.tokenSource = c.opts.TokenSource
			return
		}
		clog.Warningf(ctx, "failed to get perRPCCredentials for %q: %v", c.uri, err)
	}
	var t string
	var email string
	ts := c.opts.TokenSource

	// TODO: Use ctx directly when https://github.com/golang/oauth2/issues/262 is fixed.
	// We run ts.Token() in a goroutine because it doesn't take a context and might
	// block indefinitely, allowing us to abort early if ctx is canceled.
	var tok *oauth2.Token
	var err error
	ch := make(chan struct{})
	go func() {
		tok, err = ts.Token()
		close(ch)
	}()

	select {
	case <-ctx.Done():
		c.err = context.Cause(ctx)
		return
	case <-ch:
	}

	if err != nil {
		if errors.Is(err, errNoAuthorization) {
			if ch, ok := ts.(*credHelperGoogle); ok {
				t = ch.h.path
				clog.Warningf(ctx, "use auth %s, no token source %v", ch.h.path, err)
			} else {
				t = fmt.Sprintf("%T", ts)
				clog.Warningf(ctx, "use auth %T, no token source: %v", ts, err)
			}
			ts = nil
		} else {
			switch c.opts.Type {
			case "luci-auth", "gcloud", "":
				clog.Warningf(ctx, "auth %s: %v", c.opts.Type, err)
				c.err = fmt.Errorf("need to run `siso login`")
				return
			default:
				c.err = err
				return
			}
		}
	} else {
		t, _ = tok.Extra("x-token-source").(string)
		email, _ = tok.Extra("x-token-email").(string)
		clog.Infof(ctx, "use auth %v email: %s", t, email)
		ts = oauth2.ReuseTokenSource(tok, ts)
	}
	perRPCCredentials := c.opts.PerRPCCredentials
	if perRPCCredentials == nil {
		if ts != nil {
			perRPCCredentials = oauth.TokenSource{
				TokenSource: ts,
			}
		}
	}
	c.Type = t
	c.Email = email
	c.perRPCCredentials = perRPCCredentials
	c.tokenSource = ts
}

func (c *lazyCredWrapper) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	c.wg.Wait()
	if c.err != nil {
		return nil, c.err
	}
	if c.perRPCCredentials != nil {
		return c.perRPCCredentials.GetRequestMetadata(ctx, uri...)
	}
	return nil, nil
}

func (c *lazyCredWrapper) RequireTransportSecurity() bool {
	c.wg.Wait()
	if c.perRPCCredentials != nil {
		return c.perRPCCredentials.RequireTransportSecurity()
	}
	return true
}

func (c *lazyCredWrapper) Token() (*oauth2.Token, error) {
	c.wg.Wait()
	if c.err != nil {
		return nil, c.err
	}
	if c.tokenSource != nil {
		return c.tokenSource.Token()
	}
	return nil, fmt.Errorf("no token source")
}
