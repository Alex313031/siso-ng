// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package cachestore provides the interface CacheStore. It's in its own package
// to break a dependency loop.
package cachestore

import (
	"context"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/reapi/digest"
)

// CacheStore is an interface of cache store.
type CacheStore interface {
	// GetActionResult gets the action result of the action identified by the digest.
	GetActionResult(context.Context, digest.Digest) (*rpb.ActionResult, error)
	// SetActionResult gets the action result of the action identified by the digest.
	// If a failing action is provided, caching will be skipped.
	SetActionResult(context.Context, digest.Digest, *rpb.ActionResult) error

	// GetContent gets the content of the file identified by the digest.
	GetContent(context.Context, digest.Digest, string) ([]byte, error)
	// SetContent sets the content of the file identified by the digest.
	SetContent(context.Context, digest.Digest, string, []byte) error
	// HasContent checks whether content of the digest exists in the cache.
	HasContent(context.Context, digest.Digest) bool

	// Source returns digest source for the name identified by the digest.
	Source(context.Context, digest.Digest, string) digest.Source
}
