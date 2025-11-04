// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package signals makes it easier to catch interrupts.
package signals

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.chromium.org/build/siso/o11y/clog"
)

// HandleInterrupt calls fn in a separate goroutine on interrupt
// (SIGTERM or Ctrl-C).
//
// When interrupt comes for a second time, logs and kills
// the process immediately via os.Exit(1).
//
// Returns a callback that can be used to remove the installed signal handlers.
func HandleInterrupt(ctx context.Context, fn func()) func() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	go func() {
		handled := false
		for range ch {
			if handled {
				clog.Exitf(ctx, "Got second interrupt signal. Aborting.")
			}
			handled = true
			go fn()
		}
	}()

	return func() {
		signal.Stop(ch)
		close(ch)
	}
}
