// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package soongutil_test

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/toolsupport/soongutil"
	pb "go.chromium.org/build/siso/toolsupport/soongutil/proto"
)

type fakeStepDef struct {
	tags string
}

func (fakeStepDef) String() string                { return "fake" }
func (fakeStepDef) Next() build.StepDef           { return nil }
func (fakeStepDef) EnsureRule(context.Context)    {}
func (fakeStepDef) RuleName() string              { return "" }
func (fakeStepDef) ActionName() string            { return "fake" }
func (fakeStepDef) Args(context.Context) []string { return nil }
func (fakeStepDef) IsPhony() bool                 { return false }
func (fakeStepDef) CmdHash() []byte               { return []byte("<cmdhash>") }
func (f fakeStepDef) Binding(name string) string {
	switch name {
	case "tags":
		return f.tags
	}
	return ""
}

func (fakeStepDef) Depfile(context.Context) string         { return "" }
func (fakeStepDef) Rspfile(context.Context) string         { return "" }
func (fakeStepDef) Inputs(context.Context) []string        { return nil }
func (fakeStepDef) TriggerInputs(context.Context) []string { return nil }
func (fakeStepDef) DepInputs(context.Context) (iter.Seq[string], error) {
	return func(yield func(string) bool) {}, nil
}
func (fakeStepDef) DepsBaseInputs(ctx context.Context, inputs []string, includeOrderOnly bool) []string {
	return inputs
}
func (fakeStepDef) ToolInputs(context.Context) []string                              { return nil }
func (fakeStepDef) ExpandedCaseSensitives(ctx context.Context, in []string) []string { return in }
func (fakeStepDef) ExpandedInputs(ctx context.Context) []string                      { return nil }
func (fakeStepDef) RemoteInputs() map[string]string                                  { return nil }
func (fakeStepDef) CheckInputDeps(context.Context, []string) (bool, error)           { return false, nil }
func (fakeStepDef) Handle(context.Context, *execute.Cmd) error                       { return nil }
func (fakeStepDef) Outputs(context.Context) []string                                 { return nil }
func (fakeStepDef) AuxiliaryLogOutputFiles(context.Context) []string                 { return nil }
func (fakeStepDef) AuxiliaryLogOutputDirs(context.Context) []string                  { return nil }
func (fakeStepDef) LocalOutputs(context.Context) []string                            { return nil }
func (fakeStepDef) Pure() bool                                                       { return false }
func (fakeStepDef) Platform() map[string]string                                      { return nil }
func (fakeStepDef) Sandbox() map[string]string                                       { return nil }
func (fakeStepDef) RecordDeps(context.Context, string, time.Time, digest.Digest, []string) (bool, error) {
	return false, nil
}
func (fakeStepDef) RuleFix(context.Context, []string, []string) []byte { return nil }

func getStatus(t *testing.T, buf *bytes.Buffer) *pb.Status {
	t.Helper()
	data := buf.Bytes()
	if len(data) == 0 {
		t.Fatalf("expected output, got empty buffer")
	}

	// Read length-delimited message
	size, n := protowire.ConsumeVarint(data)
	if n < 0 {
		t.Fatalf("bad varint size")
	}

	data = data[n:]
	if uint64(len(data)) != size {
		t.Fatalf("expected size %d, got %d available", size, len(data))
	}

	msg := &pb.Status{}
	if err := proto.Unmarshal(data, msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	return msg
}
func validateTags(t *testing.T, got, want *string) {
	t.Helper()
	// Return either `nil` or a dereferenced, quoted string.
	quotedOrNil := func(p *string) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprintf("%q", *p)
	}
	if qGot, qWant := quotedOrNil(got), quotedOrNil(want); qGot != qWant {
		t.Errorf("got %s, want %s", qGot, qWant)
	}
}

func TestFrontendEdgeFinishedTags(t *testing.T) {

	for _, tc := range []struct {
		name string
		tags string
		want *string
	}{
		{
			name: "finished_with_tags",
			tags: "module:example,type:android",
			want: proto.String("module:example,type:android"),
		},
		{
			name: "finished_empty_tags",
			tags: "",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := t.Context()
			fe := soongutil.NewFrontend(ctx, &buf)

			step := build.NewStepForTest(12345, fakeStepDef{
				tags: tc.tags,
			})

			fe.BuildActionFinished(step)
			fe.Close()

			msg := getStatus(t, &buf)
			ef := msg.GetEdgeFinished()
			if ef == nil {
				t.Fatalf("edge_finished not set")
			}

			validateTags(t, ef.Tags, tc.want)
		})
	}
}

func TestFrontendEdgeCanceledTags(t *testing.T) {

	for _, tc := range []struct {
		name string
		tags string
		want *string
	}{
		{
			name: "canceled_with_tags",
			tags: "module:test,type:cc",
			want: proto.String("module:test,type:cc"),
		},
		{
			name: "canceled_empty_tags",
			tags: "",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := t.Context()
			fe := soongutil.NewFrontend(ctx, &buf)

			step := build.NewStepForTest(54321, fakeStepDef{
				tags: tc.tags,
			})

			fe.BuildActionCanceled(step)
			fe.Close()

			msg := getStatus(t, &buf)
			ef := msg.GetEdgeFinished()
			if ef == nil {
				t.Fatalf("edge_finished not set")
			}

			validateTags(t, ef.Tags, tc.want)
		})
	}
}

func TestFrontendEdgeFinishedMetrics(t *testing.T) {

	for _, tc := range []struct {
		name       string
		utime      time.Duration
		stime      time.Duration
		majflt     int64
		maxRss     int64
		isLocal    bool
		wantUser   uint32
		wantSys    uint32
		wantMajflt uint64
	}{
		{
			name:       "metrics_with_values_local",
			utime:      5 * time.Second,
			stime:      2 * time.Second,
			majflt:     42,
			maxRss:     102400,
			isLocal:    true,
			wantUser:   5000,
			wantSys:    2000,
			wantMajflt: 42,
		},
		{
			name:       "metrics_with_values_remote",
			utime:      5 * time.Second,
			stime:      2 * time.Second,
			majflt:     42,
			maxRss:     102400,
			isLocal:    false,
			wantUser:   5000,
			wantSys:    2000,
			wantMajflt: 42,
		},
		{
			name:       "metrics_empty",
			utime:      0,
			stime:      0,
			majflt:     0,
			maxRss:     0,
			isLocal:    false,
			wantUser:   0,
			wantSys:    0,
			wantMajflt: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := t.Context()
			fe := soongutil.NewFrontend(ctx, &buf)

			step := build.NewStepForTest(12345, fakeStepDef{})
			step.SetMetricsForTest(build.StepMetric{
				Utime:   build.IntervalMetric(tc.utime),
				Stime:   build.IntervalMetric(tc.stime),
				Majflt:  tc.majflt,
				MaxRSS:  tc.maxRss,
				IsLocal: tc.isLocal,
			})

			fe.BuildActionFinished(step)
			fe.Close()

			msg := getStatus(t, &buf)
			ef := msg.GetEdgeFinished()
			if ef == nil {
				t.Fatalf("edge_finished not set")
			}

			if ef.GetUserTime() != tc.wantUser {
				t.Errorf("ef.GetUserTime()=%d; want=%d", ef.GetUserTime(), tc.wantUser)
			}
			if ef.GetSystemTime() != tc.wantSys {
				t.Errorf("ef.GetSystemTime()=%d; want=%d", ef.GetSystemTime(), tc.wantSys)
			}
			if ef.GetMajorPageFaults() != tc.wantMajflt {
				t.Errorf("ef.GetMajorPageFaults()=%d; want=%d", ef.GetMajorPageFaults(), tc.wantMajflt)
			}
			wantMaxRss := uint64(tc.maxRss / 1024)
			if ef.GetMaxRssKb() != wantMaxRss {
				t.Errorf("ef.GetMaxRssKb()=%d; want=%d", ef.GetMaxRssKb(), wantMaxRss)
			}
		})
	}
}

func TestFrontendEdgeCanceledMetrics(t *testing.T) {

	for _, tc := range []struct {
		name       string
		utime      time.Duration
		stime      time.Duration
		majflt     int64
		maxRss     int64
		isLocal    bool
		wantUser   uint32
		wantSys    uint32
		wantMajflt uint64
	}{
		{
			name:       "canceled_metrics_with_values_local",
			utime:      5 * time.Second,
			stime:      2 * time.Second,
			majflt:     42,
			maxRss:     102400,
			isLocal:    true,
			wantUser:   5000,
			wantSys:    2000,
			wantMajflt: 42,
		},
		{
			name:       "canceled_metrics_with_values_remote",
			utime:      5 * time.Second,
			stime:      2 * time.Second,
			majflt:     42,
			maxRss:     102400,
			isLocal:    false,
			wantUser:   5000,
			wantSys:    2000,
			wantMajflt: 42,
		},
		{
			name:       "canceled_metrics_empty",
			utime:      0,
			stime:      0,
			majflt:     0,
			maxRss:     0,
			isLocal:    false,
			wantUser:   0,
			wantSys:    0,
			wantMajflt: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := t.Context()
			fe := soongutil.NewFrontend(ctx, &buf)

			step := build.NewStepForTest(54321, fakeStepDef{})
			step.SetMetricsForTest(build.StepMetric{
				Utime:   build.IntervalMetric(tc.utime),
				Stime:   build.IntervalMetric(tc.stime),
				Majflt:  tc.majflt,
				MaxRSS:  tc.maxRss,
				IsLocal: tc.isLocal,
			})

			fe.BuildActionCanceled(step)
			fe.Close()

			msg := getStatus(t, &buf)
			ef := msg.GetEdgeFinished()
			if ef == nil {
				t.Fatalf("edge_finished not set")
			}

			if ef.GetUserTime() != tc.wantUser {
				t.Errorf("ef.GetUserTime()=%d; want=%d", ef.GetUserTime(), tc.wantUser)
			}
			if ef.GetSystemTime() != tc.wantSys {
				t.Errorf("ef.GetSystemTime()=%d; want=%d", ef.GetSystemTime(), tc.wantSys)
			}
			if ef.GetMajorPageFaults() != tc.wantMajflt {
				t.Errorf("ef.GetMajorPageFaults()=%d; want=%d", ef.GetMajorPageFaults(), tc.wantMajflt)
			}
			wantMaxRss := uint64(tc.maxRss / 1024)
			if ef.GetMaxRssKb() != wantMaxRss {
				t.Errorf("ef.GetMaxRssKb()=%d; want=%d", ef.GetMaxRssKb(), wantMaxRss)
			}
			if !ef.GetCanceled() {
				t.Errorf("ef.GetCanceled()=false; want=true")
			}
		})
	}
}
