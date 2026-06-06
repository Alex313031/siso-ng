// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/ui"
)

func (b *Builder) setupRSP(ctx context.Context, step *Step) error {
	rsp := step.cmd.RSPFile
	if rsp == "" {
		return nil
	}
	ctx, span := trace.NewSpan(ctx, "setup-rsp")
	defer span.Close(nil)
	content := step.cmd.RSPFileContent
	if log.V(1) {
		clog.Infof(ctx, "create rsp %q=%q", rsp, content)
	}
	if experiments.Enabled("allow-unexpected-rsp-remove", "") {
		// remove before write to make sure write content to the disk
		// to avoid chtimes error with "no such file or directory"
		// when rsp was removed by some other action. b/479933778
		_, herr := b.hashFS.Stat(ctx, step.cmd.WorkspaceRoot, rsp)
		_, lerr := b.hashFS.OS.Lstat(ctx, filepath.Join(step.cmd.WorkspaceRoot, rsp))
		if herr == nil && errors.Is(lerr, fs.ErrNotExist) {
			clog.Warningf(ctx, "unexpected rsp remove detected %q", rsp)
			b.hashFS.Forget(ctx, step.cmd.WorkspaceRoot, []string{rsp})
			_, herr := b.hashFS.Stat(ctx, step.cmd.WorkspaceRoot, rsp)
			if !errors.Is(herr, fs.ErrNotExist) {
				clog.Warningf(ctx, "forget, but hashfs detect %q? %v", rsp, herr)
			}
		}
	}
	err := b.hashFS.WriteFile(ctx, step.cmd.WorkspaceRoot, rsp, content, false, time.Now(), nil, nil)
	if err != nil {
		return fmt.Errorf("failed to create rsp %s: %w", rsp, err)
	}
	return nil
}

func (b *Builder) teardownRSP(ctx context.Context, step *Step) {
	if b.keepRSP {
		rsp := step.cmd.RSPFile
		if rsp != "" {
			// setupRSP creates rsp file in hashFS memory, but it might not be flushed to disk
			// if the command was not executed locally (e.g. cache hit).
			// So we need to explicitly flush it to disk here to keep it.
			err := b.hashFS.Flush(ctx, step.cmd.WorkspaceRoot, []string{rsp})
			if err != nil {
				clog.Warningf(ctx, "failed to flush %s: %v", rsp, err)
				if !errors.Is(err, context.Canceled) {
					ui.Default.Warningf("failed to flush %s: %v\n", rsp, err)
				}
			}
		}
		return
	}
	rsp := step.cmd.RSPFile
	if rsp == "" {
		return
	}
	if log.V(1) {
		clog.Infof(ctx, "remove rsp %q", rsp)
	}
	err := b.hashFS.Remove(ctx, step.cmd.WorkspaceRoot, rsp)
	if err != nil {
		clog.Warningf(ctx, "failed to remove %s: %v", rsp, err)
	}
	// remove local file if it is used on local?
	err = os.Remove(filepath.Join(step.cmd.WorkspaceRoot, rsp))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		clog.Warningf(ctx, "failed to remove %s: %v", rsp, err)
	}
}
