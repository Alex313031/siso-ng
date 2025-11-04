// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/retry"
)

// ErrBadPlatformContainerImage is an error if the request used bad platform container image.
var ErrBadPlatformContainerImage = errors.New("reapi: bad platform container image")

// ExecuteAndWait executes a cmd and waits for the result.
func (c *Client) ExecuteAndWait(ctx context.Context, req *rpb.ExecuteRequest, opts ...grpc.CallOption) (string, *rpb.ExecuteResponse, error) {
	clog.Infof(ctx, "execute action")

	if req.InstanceName == "" {
		req.InstanceName = c.opt.Instance
	}

	if req.ExecutionPolicy == nil && c.opt.ExecutionPriority != 0 {
		req.ExecutionPolicy = &rpb.ExecutionPolicy{
			Priority: int32(c.opt.ExecutionPriority),
		}
	}

	var opName string
	var waitReq *rpb.WaitExecutionRequest
	resp := &rpb.ExecuteResponse{}
	type responseStream interface {
		Recv() (*longrunningpb.Operation, error)
	}
	execClient := rpb.NewExecutionClient(c.conn)
	var err error
	pctx := ctx
	var backoff retry.ExponentialBackoff
retryLoop:
	for i := 0; ; i++ {
		err = func() error {
			var stream responseStream
			var err error
			if !c.opt.KeepExecStream {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(pctx, 1*time.Minute)
				defer cancel()
				if waitReq != nil {
					stream, err = execClient.WaitExecution(ctx, waitReq, opts...)
				} else {
					stream, err = execClient.Execute(ctx, req, opts...)
				}
			} else {
				stream, err = execClient.Execute(ctx, req, opts...)
			}
			if err != nil {
				return status.Errorf(status.Code(err), "execute: %v", err)
			}
			for {
				op, err := stream.Recv()
				if err != nil {
					if status.Code(err) == codes.NotFound {
						waitReq = nil
						return status.Errorf(codes.Unavailable, "operation stream lost: %v", err)
					}
					return err
				}
				if opName == "" {
					opName = op.GetName()
					clog.Infof(ctx, "operation starts: %s", opName)
				}
				if !op.GetDone() {
					waitReq = &rpb.WaitExecutionRequest{
						Name: opName,
					}

					metadata := &rpb.ExecuteOperationMetadata{}
					err = op.GetMetadata().UnmarshalTo(metadata)
					if err != nil {
						clog.Warningf(ctx, "failed to unmarshal metadata: %v", err)
					} else {
						clog.Infof(ctx, "operation stage: %v %s", metadata.GetStage(), ongoingDetails(metadata.GetPartialExecutionMetadata()))
						if log.V(1) {
							clog.Infof(ctx, "operation metadata: %v", metadata)
						}
					}
					continue
				}
				clog.Infof(ctx, "operation done: %s", opName)
				waitReq = nil
				err = op.GetResponse().UnmarshalTo(resp)
				if err != nil {
					err = status.Errorf(codes.Internal, "op %s response bad type %T: %v", op.GetName(), op.GetResponse(), err)
					clog.Warningf(ctx, "action digest: %s failed %v", req.ActionDigest, err)
					return err
				}
				return erespErr(ctx, resp)
			}
		}()
		c.m.OpsDone(err)
		// https://github.com/bazelbuild/bazel/blob/7.1.1/src/main/java/com/google/devtools/build/lib/remote/RemoteRetrier.java#L47
		unknownErr := true
		switch status.Code(err) {
		case codes.OK:
			break retryLoop
		case codes.Canceled,
			codes.InvalidArgument,
			codes.FailedPrecondition,
			codes.Unimplemented:
			break retryLoop
		case codes.Aborted:
			// "the bot running the task appears to be lost" ?
		case codes.Internal:
			// stream terminated by RST_STREAM ?
		case codes.Unknown:
			// Authentication backend unknown error. ?
		case codes.Unavailable, codes.ResourceExhausted:
			unknownErr = false
		case codes.DeadlineExceeded:
			// ctx deadline exceeded, but pctx may not?
			unknownErr = false
		default:
			break retryLoop
		}
		select {
		case <-pctx.Done():
			clog.Warningf(pctx, "pctx done: %v", context.Cause(pctx))
			break retryLoop
		default:
			if unknownErr {
				clog.Infof(pctx, "pctx is not done: ctx=%v: %v", context.Cause(ctx), err)
			}
		}
		if status.Code(err) == codes.DeadlineExceeded {
			s, ok := status.FromError(err)
			if ok && s.Message() == "execution timeout exceeded" {
				// action timed out. no retry.
				clog.Warningf(pctx, "action timed out: %v", err)
				return opName, resp, err
			}
			// no need to backoff for deadline exceeded
			clog.Infof(pctx, "retry exec call again: %v", err)
			continue retryLoop
		}
		var delay time.Duration
		delay, err = backoff.Next(ctx, err)
		if delay == 0 {
			break
		}
		clog.Infof(pctx, "backoff %s for %v", delay, err)
		select {
		case <-pctx.Done():
			return opName, resp, context.Cause(pctx)
		case <-time.After(delay):
		}
	}
	if err == nil {
		err = status.FromProto(resp.GetStatus()).Err()
		switch status.Code(err) {
		case codes.PermissionDenied, codes.NotFound:
			// RBE returns permission denied or not found when
			// platform container image are not available
			// on RBE worker.
			err = fmt.Errorf("%w: %w", ErrBadPlatformContainerImage, err)
		}
	}
	return opName, resp, err
}

func erespErr(ctx context.Context, eresp *rpb.ExecuteResponse) error {
	st := eresp.GetStatus()
	if codes.Code(st.GetCode()) != codes.OK && len(st.GetDetails()) > 0 {
		clog.Warningf(ctx, "error details for %v: %v", codes.Code(st.GetCode()), st.GetDetails())
	}
	switch codes.Code(st.GetCode()) {
	case codes.OK:
	case codes.ResourceExhausted, codes.FailedPrecondition, codes.DeadlineExceeded:
		clog.Warningf(ctx, "execute response: status=%s", st)
		return status.FromProto(st).Err()

	case codes.Internal:
		clog.Warningf(ctx, "execute response: status=%s", st)
		if strings.Contains(st.GetMessage(), "CreateProcess: failure in a Windows system call") {
			return status.FromProto(st).Err()
		}
		// message:"docker: Error response from daemon: OCI runtime create failed: container_linux.go:380: starting container process caused: exec: \"./bin/clang++\": permission denied: unknown."
		if strings.Contains(st.GetMessage(), "runtime create failed") && strings.Contains(st.GetMessage(), "starting container process") {
			return status.FromProto(st).Err()
		}
		// message::"docker: Error response from daemon: OCI runtime start failed: starting container: starting root container: starting sandbox: creating process: failed to load /b/chromium/src/native_client/toolchain/linux_x86/nacl_x86_glibc/bin/x86_64-nacl-g++: exec format error: unknown."
		if strings.Contains(st.GetMessage(), "runtime start failed") && strings.Contains(st.GetMessage(), "exec format error") {
			return status.FromProto(st).Err()
		}
		// https://docs.google.com/document/d/1KjBEeh0rglX2BgH908TLQfE6GGBbo9DsGllB25tZuds/edit#heading=h.1o0g6cf3lax4
		// says
		// an INTERNAL or UNAVAILABLE error code are used to imply the action should be retried (perhaps the error is transient or a different bot will succeed)
		fallthrough
	case codes.Unavailable:
		st = proto.Clone(st).(*spb.Status)
		// codes.Unavailable, so that rpc.Retry will retry.
		st.Code = int32(codes.Unavailable)
		return status.FromProto(st).Err()

	case codes.Unauthenticated:
		clog.Warningf(ctx, "execute response: status=%s", st)
		if strings.Contains(st.GetMessage(), "Request had invalid authentication credentials.") {
			// may expire access token?
			st = proto.Clone(st).(*spb.Status)
			st.Code = int32(codes.Unavailable)
			return status.FromProto(st).Err()
		}
		return status.FromProto(st).Err()

	case codes.Aborted:
		if ctx.Err() == nil {
			// ctx is not canceled, but returned
			// code = Aborted, context canceled
			// in this case, it would be retriable.
			clog.Warningf(ctx, "execute response: aborted %s, but ctx is still active", st)
			if st.GetMessage() == "context canceled" {
				st = proto.Clone(st).(*spb.Status)
				// codes.Unavailable, so that rpc.Retry will retry.
				st.Code = int32(codes.Unavailable)
				return status.FromProto(st).Err()
			}
			// otherwise, "the bot running the task appears to be lost"
			return status.FromProto(st).Err()
		}
		fallthrough
	default:
		clog.Errorf(ctx, "execute response: status %s", st)
		return status.FromProto(st).Err()
	}
	return nil
}

func ongoingDetails(md *rpb.ExecutedActionMetadata) string {
	if md == nil {
		return ""
	}
	var sb strings.Builder
	if w := md.GetWorker(); w != "" {
		fmt.Fprintf(&sb, "worker=%s ", w)
	}
	if md.QueuedTimestamp == nil {
		fmt.Fprintf(&sb, "queued=unknown")
		return sb.String()
	}
	if md.WorkerStartTimestamp == nil {
		fmt.Fprintf(&sb, "queued=%s", md.GetQueuedTimestamp().AsTime())
		return sb.String()
	}
	fmt.Fprintf(&sb, "queue=%s %s ", md.GetQueuedTimestamp().AsTime(),
		md.GetWorkerStartTimestamp().AsTime().Sub(md.GetQueuedTimestamp().AsTime()))
	if md.WorkerCompletedTimestamp != nil {
		fmt.Fprintf(&sb, "worker=%s %s ", md.GetWorkerStartTimestamp().AsTime(),
			md.GetWorkerCompletedTimestamp().AsTime().Sub(md.GetWorkerStartTimestamp().AsTime()))
	}
	if md.InputFetchStartTimestamp != nil && md.InputFetchCompletedTimestamp == nil {
		fmt.Fprintf(&sb, "input=%s", md.GetInputFetchStartTimestamp().AsTime())
		return sb.String()
	}
	if md.InputFetchCompletedTimestamp != nil {
		fmt.Fprintf(&sb, "input=%s ", md.GetInputFetchCompletedTimestamp().AsTime().Sub(md.GetInputFetchStartTimestamp().AsTime()))
	}
	if md.ExecutionStartTimestamp != nil && md.ExecutionCompletedTimestamp == nil {
		fmt.Fprintf(&sb, "exec=%s", md.GetExecutionStartTimestamp().AsTime())
		return sb.String()
	}
	if md.ExecutionCompletedTimestamp != nil {
		fmt.Fprintf(&sb, "exec=%s ", md.GetExecutionCompletedTimestamp().AsTime().Sub(md.GetExecutionStartTimestamp().AsTime()))
	}
	if md.OutputUploadStartTimestamp != nil && md.OutputUploadCompletedTimestamp == nil {
		fmt.Fprintf(&sb, "output=%s", md.GetOutputUploadStartTimestamp().AsTime())
	}
	if md.OutputUploadCompletedTimestamp != nil {
		fmt.Fprintf(&sb, "output=%s ", md.GetOutputUploadCompletedTimestamp().AsTime().Sub(md.GetOutputUploadStartTimestamp().AsTime()))
	}
	return sb.String()
}
