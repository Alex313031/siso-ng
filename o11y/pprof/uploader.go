// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package pprof

import (
	"bytes"
	"context"
	"fmt"
	"regexp"

	pb "google.golang.org/genproto/googleapis/devtools/cloudprofiler/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.chromium.org/build/siso/build/metadata"
	"go.chromium.org/build/siso/o11y/clog"
)

// Options is a profile uploader option.
type Options struct {
	ProjectID   string
	DialOptions []grpc.DialOption
}

// Uploader is a profile uploader.
type Uploader struct {
	projectID   string
	dialOptions []grpc.DialOption

	target string
	labels map[string]string
}

// NewUploader creates new profile uploader.
func NewUploader(ctx context.Context, opts Options) (*Uploader, error) {
	p := &Uploader{
		projectID:   opts.ProjectID,
		dialOptions: opts.DialOptions,
		target:      "siso-build",
		labels:      map[string]string{},
	}
	return p, nil
}

var validLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// SetMetadata sets metadata to the uploader.
func (u *Uploader) SetMetadata(ctx context.Context, metadata metadata.Metadata) {
	if u == nil {
		return
	}
	for _, k := range metadata.Keys() {
		if k == "product" {
			u.target = fmt.Sprintf("siso-build.%s", metadata.Get(k))
			clog.Infof(ctx, "pprof target=%q", u.target)
			continue
		}
		if !validLabelRE.MatchString(k) {
			clog.Warningf(ctx, "invalid label for pprof: %q", k)
			continue
		}
		u.labels[k] = metadata.Get(k)
		clog.Infof(ctx, "pprof label %q=%q", k, metadata.Get(k))
	}
}

// Upload uploads a profile.
func (u *Uploader) Upload(ctx context.Context, p *Profile) error {
	if u == nil || u.projectID == "" {
		return nil
	}
	var buf bytes.Buffer
	err := p.WriteTo(&buf, 0)
	if err != nil {
		return err
	}

	// better to configure retry etc by service_config??
	conn, err := grpc.NewClient("cloudprofiler.googleapis.com:443", u.dialOptions...)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewProfilerServiceClient(conn)
	resp, err := client.CreateOfflineProfile(ctx, &pb.CreateOfflineProfileRequest{
		Parent: u.projectID,
		Profile: &pb.Profile{
			ProfileType: pb.ProfileType_WALL,
			// https://github.com/googleapis/googleapis/blob/f694606e3e34d0e2a289d655864e225cab9ecb88/google/devtools/cloudprofiler/v2/profiler.proto#L156
			Deployment: &pb.Deployment{
				ProjectId: u.projectID,
				// better to add product name in target?
				Target: u.target,
				Labels: u.labels,
			},
			Duration:     durationpb.New(p.Duration()),
			ProfileBytes: buf.Bytes(),
		},
	})
	if err != nil {
		return err
	}
	clog.Infof(ctx, "pprof uploaded: %s", resp)
	return nil
}
