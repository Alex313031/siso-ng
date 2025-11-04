// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package clog_test is a test for clog package.
package clog_test

import (
	"fmt"
	"sync"
	"testing"

	"cloud.google.com/go/logging"

	"go.chromium.org/build/siso/o11y/clog"
)

func testFormater(e logging.Entry) string {
	id := e.Labels["id"]

	if id == "" {
		return fmt.Sprintf("%v", e.Payload)
	}
	return fmt.Sprintf("%s %v", id, e.Payload)
}

func Test(t *testing.T) {
	ctx := t.Context()

	l := clog.FromContext(ctx)
	defer l.Close()

	clog.Infof(ctx, "Info")
	clog.Warningf(ctx, "Warning")
	clog.Errorf(ctx, "Error")

	l.Formatter = testFormater

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cctx := clog.NewSpan(ctx, "trace1", "span1", map[string]string{
			"id": "id1"})

		clog.Infof(cctx, "Child Info")
		clog.Warningf(cctx, "Child Warning")
		clog.Errorf(cctx, "Child Error")
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		cctx := clog.NewSpan(ctx, "trace2", "span2", map[string]string{
			"id": "id2"})

		clog.Infof(cctx, "Child Info")
		clog.Warningf(cctx, "Child Warning")
		clog.Errorf(cctx, "Child Error")
	}()
	wg.Wait()

	// TODO(b/267409605): Add assertions to check for the generated log contents.
}
