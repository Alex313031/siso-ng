// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package soongutil

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/o11y/clog"
	pb "go.chromium.org/build/siso/toolsupport/soongutil/proto"
	"go.chromium.org/build/siso/ui"
)

// Frontend implements build.StatusReporter and ui.UI and
// is used to report build status to soong.
// It provides same functionalities as Android ninja's StatusSerializer
// https://android.googlesource.com/platform/external/ninja/+/75a0997a4be6439f824ef84834dbc9ab76ab34e5/src/status.cc
// soong will read status updates via frontend FIFO file
// proto/frontend.proto's Status message
// by https://android.googlesource.com/platform/build/soong/+/refs/heads/main/ui/status/ninja.go
// and show to the terminal by
// https://android.googlesource.com/platform/build/soong/+/refs/heads/main/ui/terminal/smart_status.go
type Frontend struct {
	w io.Writer

	startTime time.Time

	ch   chan *pb.Status
	quit chan struct{}
	done chan struct{}

	mu       sync.Mutex
	building bool
	total    int
}

// NewFrontend creates new frontend to write to w.
func NewFrontend(ctx context.Context, w io.Writer) *Frontend {
	f := &Frontend{
		w:         w,
		startTime: time.Now(),
		ch:        make(chan *pb.Status, 10000),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go f.run(ctx)
	return f
}

func (f *Frontend) run(ctx context.Context) {
	defer close(f.done)
	var buf []byte
	for {
		select {
		case <-f.quit:
			return
		case m := <-f.ch:
			// Send the proto as a length-delimited message.
			// size as Varint without tag.
			b, err := proto.Marshal(m)
			if err != nil {
				clog.Warningf(ctx, "failed to marshal: %v", err)
				continue
			}
			n := uint64(len(b))
			buf = buf[:0]
			buf = protowire.AppendVarint(buf, n)
			buf = append(buf, b...)
			_, err = f.w.Write(buf)
			if err != nil {
				clog.Warningf(ctx, "failed to send status: %v", err)
				return
			}
		}
	}
}

// Close closes the frontend and finishes writing.
func (f *Frontend) Close() {
	close(f.quit)
	<-f.done
}

// PlanHasTotalSteps is called when total steps is updated.
func (f *Frontend) PlanHasTotalSteps(total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if total == f.total {
		return
	}
	f.total = total
	m := &pb.Status{
		TotalEdges: &pb.Status_TotalEdges{
			TotalEdges: proto.Uint32(uint32(f.total)),
		},
	}
	f.ch <- m
}

// BuildActionStarted is called when build action started.
func (f *Frontend) BuildActionStarted(step *build.Step) {
	m := &pb.Status{
		EdgeStarted: &pb.Status_EdgeStarted{
			Id:        proto.Uint32(uint32(step.IDNum())),
			StartTime: proto.Uint32(uint32(time.Since(f.startTime).Milliseconds())),
			Outputs:   step.Outputs(),
			Desc:      proto.String(step.Desc()),
			Command:   proto.String(step.Command()),
			Console:   proto.Bool(step.IsConsole()),
			// TODO: pass more info?
		},
	}
	f.ch <- m
}

// BuildActionFinished is called when build action finished.
func (f *Frontend) BuildActionFinished(step *build.Step) {
	m := &pb.Status{
		EdgeFinished: &pb.Status_EdgeFinished{
			Id:      proto.Uint32(uint32(step.IDNum())),
			EndTime: proto.Uint32(uint32(time.Since(f.startTime).Milliseconds())),
			Status:  proto.Int32(step.ExitCode()),
			Output:  proto.String(step.OutputResult()),
			// TODO: pass more info?
		},
	}
	f.ch <- m
}

// BuildActionCanceled is called when build action canceled.
func (f *Frontend) BuildActionCanceled(step *build.Step) {
	m := &pb.Status{
		EdgeFinished: &pb.Status_EdgeFinished{
			Id:       proto.Uint32(uint32(step.IDNum())),
			EndTime:  proto.Uint32(uint32(time.Since(f.startTime).Milliseconds())),
			Status:   proto.Int32(-1),
			Canceled: proto.Bool(true),
		},
	}
	f.ch <- m
}

// BuildStarted is called when build started.
// Once started, PrintLines will be suppressed to ignore build progress
// since these are reported bia BuildActionStarted / BuildActionFinished.
func (f *Frontend) BuildStarted() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.building = true
	// TODO: pass more info?
	m := &pb.Status{
		BuildStarted: &pb.Status_BuildStarted{},
	}
	f.ch <- m
}

// BuildFinished is called when build finished.
func (f *Frontend) BuildFinished() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.building = false
	m := &pb.Status{
		BuildFinished: &pb.Status_BuildFinished{},
	}
	f.ch <- m
}

func (f *Frontend) message(level pb.Status_Message_Level, msg string) {
	m := &pb.Status{
		Message: &pb.Status_Message{
			Level:   level.Enum(),
			Message: proto.String(msg),
		},
	}
	f.ch <- m
}

// PrintLines reports message lines at info level.
func (f *Frontend) PrintLines(msgs ...string) {
	f.mu.Lock()
	building := f.building
	f.mu.Unlock()
	if building {
		return
	}
	f.message(pb.Status_Message_INFO, strings.Join(msgs, "\n"))
}

type frontendSpinner struct {
	f   *Frontend
	msg string
}

func (s frontendSpinner) Start(format string, args ...any) {
	s.msg = fmt.Sprintf(format, args...)
	s.f.message(pb.Status_Message_DEBUG, s.msg)
}

func (s frontendSpinner) Stop(err error) {
	if err != nil {
		s.f.message(pb.Status_Message_DEBUG, fmt.Sprintf("%s failed: %v\n", s.msg, err))
		return
	}
	s.f.message(pb.Status_Message_DEBUG, s.msg+"\n")
}

func (s frontendSpinner) Done(format string, args ...any) {
	s.f.message(pb.Status_Message_DEBUG, s.msg+" "+fmt.Sprintf(format, args...)+"\n")
}

// NewSpinner returns a new spinner, which reports message at debug level.
func (f *Frontend) NewSpinner() ui.Spinner {
	return frontendSpinner{f: f}
}

// Infof reports info level.
func (f *Frontend) Infof(format string, args ...any) {
	f.message(pb.Status_Message_INFO, fmt.Sprintf(format, args...))
}

// Warningf reports warning level.
func (f *Frontend) Warningf(format string, args ...any) {
	f.message(pb.Status_Message_WARNING, fmt.Sprintf(format, args...))
}

// Errorf reports error level.
func (f *Frontend) Errorf(format string, args ...any) {
	f.message(pb.Status_Message_ERROR, fmt.Sprintf(format, args...))
}
