// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package localexec

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
	pb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/o11y/clog"
)

// external tap command
var tapCommand = os.Getenv("SISO_TAP_COMMAND")

// tapCmd converts cmd to use external tapping.
// It also returns function that sets tap result in action result's metadata,
// and cleans up temporary file.
func tapCmd(cmd *execute.Cmd) (*execute.Cmd, func(context.Context, *rpb.ActionResult) error, error) {
	if tapCommand == "" {
		return cmd, func(context.Context, *rpb.ActionResult) error { return nil }, nil
	}
	tcmd := cmd.Clone()
	tapLogFile, err := os.CreateTemp("", fmt.Sprintf("tap-%s-*.json", tcmd.ID))
	if err != nil {
		return nil, nil, err
	}
	_ = tapLogFile.Close()
	postProcess := func(ctx context.Context, res *rpb.ActionResult) error {
		err := attachTapResult(tapLogFile.Name(), res)
		if rerr := os.Remove(tapLogFile.Name()); rerr != nil {
			clog.Warningf(ctx, "failed to remove tap tempfile %q: %v", tapLogFile.Name(), rerr)
		}
		return err
	}
	tcmd.Args = append([]string{
		tapCommand,
		"--tap_output", tapLogFile.Name(),
		"--",
	}, cmd.Args...)
	return tcmd, postProcess, nil
}

// attach tap result stored as jsonformat in tapLogFilename
// to action result's auxiliary metadata.
func attachTapResult(tapLogFileName string, res *rpb.ActionResult) error {
	if res.ExitCode != 0 {
		return nil
	}
	buf, err := os.ReadFile(tapLogFileName)
	if err != nil {
		return err
	}
	tapData := &pb.TapResult{}
	err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(buf, tapData)
	if err != nil {
		tapData.Error = err.Error()
	}
	any, err := anypb.New(tapData)
	if err != nil {
		return err
	}
	if res.ExecutionMetadata == nil {
		res.ExecutionMetadata = &rpb.ExecutedActionMetadata{}
	}
	res.ExecutionMetadata.AuxiliaryMetadata = append(res.ExecutionMetadata.AuxiliaryMetadata, any)
	return nil
}

// ExtractTaResult exptracts tap result from action result's metadata.
func ExtractTapResult(res *rpb.ActionResult) (*pb.TapResult, bool) {
	tapData := &pb.TapResult{}
	for _, am := range res.GetExecutionMetadata().GetAuxiliaryMetadata() {
		if am.MessageIs(tapData) {
			err := am.UnmarshalTo(tapData)
			if err != nil {
				continue
			}
			return tapData, true
		}
	}
	return nil, false
}
