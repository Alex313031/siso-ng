// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package resultstore uploads to resultstore.
package resultstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
	rspb "google.golang.org/genproto/googleapis/devtools/resultstore/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
)

// Options is options for resultstore uploader.
type Options struct {
	InvocationID  string
	Invocation    *rspb.Invocation
	ClientOptions []option.ClientOption
}

// Uploader is resultstore uploader.
type Uploader struct {
	// These are needed for upload and associate files in resultstore.
	HashFS      *hashfs.HashFS
	REAPIClient *reapi.Client

	conn   *grpc.ClientConn
	client rspb.ResultStoreUploadClient

	invocationID string

	buildLogMu sync.Mutex
	buildLog   bytes.Buffer

	q    chan any
	quit chan int
	done chan struct{}
}

// New creates new resultstore uploader.
func New(ctx context.Context, opts Options) (*Uploader, error) {
	copts := slices.Concat([]option.ClientOption{
		option.WithEndpoint("resultstore.googleapis.com:443"),
	}, opts.ClientOptions)
	conn, err := transport.DialGRPC(ctx, copts...)
	if err != nil {
		return nil, err
	}
	uploader := &Uploader{
		conn:   conn,
		client: rspb.NewResultStoreUploadClient(conn),

		invocationID: opts.InvocationID,

		q:    make(chan any, 100),
		quit: make(chan int),
		done: make(chan struct{}),
	}
	// Preallocating 10 MB should be enough for typical builds.
	uploader.buildLog.Grow(10 * 1024 * 1024)

	authToken := uuid.New().String()
	resumeToken := newResumeToken()

	invocation := opts.Invocation
	invocation.StatusAttributes = &rspb.StatusAttributes{
		Status: rspb.Status_BUILDING,
	}
	if invocation.Timing == nil || invocation.Timing.StartTime == nil {
		invocation.Timing = &rspb.Timing{
			StartTime: timestamppb.Now(),
		}
	}

	req := &rspb.CreateInvocationRequest{
		InvocationId:       opts.InvocationID,
		Invocation:         invocation,
		AuthorizationToken: authToken,
		InitialResumeToken: resumeToken,
	}
	_, err = uploader.client.CreateInvocation(ctx, req)
	if err != nil {
		cerr := conn.Close()
		if err != nil {
			clog.Warningf(ctx, "close conn: %v", cerr)
		}
		return nil, err
	}
	go uploader.run(context.Background(), req)
	return uploader, nil
}

// Close closes resultstore uploader to finish invocation with exitCode.
func (u *Uploader) Close(ctx context.Context, exitCode int) error {
	clog.Infof(ctx, "quit resultstore exit_code=%d", exitCode)
	u.quit <- exitCode
	<-u.done
	clog.Infof(ctx, "resultstore done")
	if u.conn == nil {
		return nil
	}
	return u.conn.Close()
}

// Upload uploads req.
func (u *Uploader) Upload(ctx context.Context, req *rspb.UploadRequest) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case u.q <- req:
	}
	return nil
}

func (u *Uploader) run(ctx context.Context, initReq *rspb.CreateInvocationRequest) {
	defer close(u.done)
	startTime := time.Now()
	if initReq.Invocation.GetTiming().GetStartTime() != nil {
		startTime = initReq.Invocation.GetTiming().GetStartTime().AsTime()
	}

	batchReq := &rspb.UploadBatchRequest{
		Parent:             fmt.Sprintf("invocations/%s", initReq.InvocationId),
		AuthorizationToken: initReq.AuthorizationToken,
		ResumeToken:        initReq.InitialResumeToken,
		NextResumeToken:    newResumeToken(),
	}
	var exitCode int
loop:
	for {
		select {
		case exitCode = <-u.quit:
			clog.Infof(ctx, "quit upload resultstore exit_code=%d", exitCode)
			break loop
		case x := <-u.q:
			switch req := x.(type) {
			case *rspb.UploadRequest:
				batchReq.UploadRequests = append(batchReq.UploadRequests, req)
				if proto.Size(batchReq) < 4*1024*1024 {
					continue
				}
				// remove last req to fit in proto size limit.
				batchReq.UploadRequests = slices.Delete(batchReq.UploadRequests, len(batchReq.UploadRequests)-1, len(batchReq.UploadRequests))
				clog.Infof(ctx, "upload resultstore %d reqs", len(batchReq.UploadRequests))
				err := u.uploadBatch(ctx, batchReq)
				if err != nil {
					clog.Warningf(ctx, "failed to upload resultstore: %v", err)
				}
				batchReq.UploadRequests = append(batchReq.UploadRequests, req)

			case *digest.Store:
				started := time.Now()
				n, err := u.REAPIClient.UploadAll(ctx, req)
				if err != nil {
					clog.Warningf(ctx, "failed to upload files: n=%d, %s: %v", n, time.Since(started), err)
				} else {
					clog.Infof(ctx, "upload files: n=%d, %s", n, time.Since(started))
				}

			default:
				clog.Warningf(ctx, "unknown request type: %T", x)
			}

		case <-time.After(1 * time.Minute):
			// flush batchReq
			if len(batchReq.UploadRequests) == 0 {
				clog.Infof(ctx, "no upload resultstore reqs")
				continue
			}
			clog.Infof(ctx, "upload resultstore %d reqs", len(batchReq.UploadRequests))
			err := u.uploadBatch(ctx, batchReq)
			if err != nil {
				clog.Warningf(ctx, "failed to upload resultstore: %v", err)
			}
		}
	}
	err := u.uploadBatch(ctx, batchReq)
	if err != nil {
		clog.Warningf(ctx, "failed to upload resultstore: %v", err)
	}

	status := rspb.Status_BUILT
	if exitCode != 0 {
		status = rspb.Status_FAILED_TO_BUILD
	}
	batchReq.UploadRequests = append(
		batchReq.UploadRequests,
		&rspb.UploadRequest{
			UploadOperation: rspb.UploadRequest_UPDATE,
			UpdateMask: &fieldmaskpb.FieldMask{
				Paths: []string{
					"status_attributes",
					"timing.duration",
					"invocation_attributes.exit_code",
				},
			},
			Resource: &rspb.UploadRequest_Invocation{
				Invocation: &rspb.Invocation{
					StatusAttributes: &rspb.StatusAttributes{
						Status: status,
					},
					Timing: &rspb.Timing{
						Duration: durationpb.New(time.Since(startTime)),
					},
					InvocationAttributes: &rspb.InvocationAttributes{
						ExitCode: int32(exitCode),
					},
				},
			},
		},
		&rspb.UploadRequest{
			UploadOperation: rspb.UploadRequest_FINALIZE,
			Resource: &rspb.UploadRequest_Invocation{
				Invocation: &rspb.Invocation{},
			},
		},
	)
	err = u.uploadBatch(ctx, batchReq)
	if err != nil {
		clog.Warningf(ctx, "failed to upload resultstore finalize: %v", err)
	}
}

func newResumeToken() string {
	v := uuid.New().String()
	return base64.URLEncoding.EncodeToString([]byte(v))
}

func (u *Uploader) uploadBatch(ctx context.Context, req *rspb.UploadBatchRequest) error {
	_, err := u.client.UploadBatch(ctx, req)
	if err != nil {
		clog.Warningf(ctx, "upload failed for %s: %v", req, err)
	}
	req.UploadRequests = slices.Delete(req.UploadRequests, 0, len(req.UploadRequests))
	req.ResumeToken = req.NextResumeToken
	req.NextResumeToken = newResumeToken()
	return err
}
