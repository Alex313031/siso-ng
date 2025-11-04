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
	err := b.hashFS.WriteFile(ctx, step.cmd.ExecRoot, rsp, content, false, time.Now(), nil, nil)
	if err != nil {
		return fmt.Errorf("failed to create rsp %s: %w", rsp, err)
	}
	return nil
}

func (b *Builder) teardownRSP(ctx context.Context, step *Step) {
	if b.keepRSP {
		return
	}
	rsp := step.cmd.RSPFile
	if rsp == "" {
		return
	}
	if log.V(1) {
		clog.Infof(ctx, "remove rsp %q", rsp)
	}
	err := b.hashFS.Remove(ctx, step.cmd.ExecRoot, rsp)
	if err != nil {
		clog.Warningf(ctx, "failed to remove %s: %v", rsp, err)
	}
	// remove local file if it is used on local?
	err = os.Remove(filepath.Join(step.cmd.ExecRoot, rsp))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		clog.Warningf(ctx, "failed to remove %s: %v", rsp, err)
	}
}
