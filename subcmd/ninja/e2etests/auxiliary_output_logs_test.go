// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_Auxiliary_Remote(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	testDataName := t.Name()

	cases := []struct {
		name     string
		exitCode int32
		wantErr  bool
	}{
		{
			name:     "success",
			exitCode: 0,
			wantErr:  false,
		},
		{
			// We expect auxiliary output paths digest to be recorded regardless of execution success or failure.
			name:     "failure",
			exitCode: 1,
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tempDir(t)
			runNinjaTest := func(t *testing.T, refake *reapitest.Fake, outputLog, failureSummaryLog *bytes.Buffer) (build.Stats, error) {
				t.Helper()
				var ds build.DataSource
				defer func() {
					err := ds.Close(ctx)
					if err != nil {
						t.Error(err)
					}
				}()
				ds.Client = reapitest.NewWithOption(ctx, t, refake, reapi.Option{
					Instance: "testinstance",
				})
				opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
					StateFile:  ".siso_fs_state",
					DataSource: ds,
				})
				defer cleanup()
				opt.REExecEnable = true
				opt.StrictRemote = true
				opt.REAPIClient = ds.Client
				opt.ProjectID = "testproject"
				opt.OutputLogWriter = outputLog
				opt.FailureSummaryWriter = failureSummaryLog
				stats, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
				return stats, err
			}

			setupFiles(t, dir, testDataName, nil)

			outContent := []byte("out-content")
			outDigest := digest.FromBytes("out.o", outContent)
			auxContent := []byte("aux-content")
			auxDigest := digest.FromBytes("debug.out", auxContent)

			auxDirFileContent := []byte("aux-dir-file-content")
			auxTree := &rpb.Tree{Root: &rpb.Directory{
				Files: []*rpb.FileNode{{
					Name:   "file",
					Digest: digest.FromBytes("aux_dir/file", auxDirFileContent).Digest().Proto(),
				}},
			}}
			auxTreeBytes, err := proto.Marshal(auxTree)
			if err != nil {
				t.Fatal(err)
			}
			auxTreeDigest := digest.FromBytes("aux_dir", auxTreeBytes)

			fakere := &reapitest.Fake{
				ExecuteFunc: func(re *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
					re.Put(ctx, auxContent)
					re.Put(ctx, outContent)
					re.Put(ctx, auxDirFileContent)
					re.Put(ctx, auxTreeBytes)
					return &rpb.ActionResult{
						ExitCode: tc.exitCode,
						OutputFiles: []*rpb.OutputFile{
							{
								Path:   "out.o",
								Digest: outDigest.Digest().Proto(),
							},
							{
								Path:   "debug.out",
								Digest: auxDigest.Digest().Proto(),
							},
						},
						OutputDirectories: []*rpb.OutputDirectory{
							{
								Path:       "aux_dir",
								TreeDigest: auxTreeDigest.Digest().Proto(),
							},
						},
					}, nil
				},
			}

			var outputLog bytes.Buffer
			var failureSummaryLog bytes.Buffer
			stats, err := runNinjaTest(t, fakere, &outputLog, &failureSummaryLog)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("ninja err=%v; wantErr=%t", err, tc.wantErr)
			}
			if stats.Remote != 1 {
				t.Errorf("stats.Remote=%d; want 1", stats.Remote)
			}

			wantAux := fmt.Sprintf(`auxiliary outputs:
out/siso/aux_dir/	%s	siso fetch -reapi_instance testinstance -type=tree-extract %s out/siso/aux_dir/
out/siso/debug.out	%s	siso fetch -reapi_instance testinstance %s out/siso/debug.out
`, auxTreeDigest.Digest(), auxTreeDigest.Digest(), auxDigest.Digest(), auxDigest.Digest())
			if !strings.Contains(outputLog.String(), wantAux) {
				t.Errorf("output log missing expected auxiliary outputs:\n%s\n\ngot:\n%s", wantAux, outputLog.String())
			}

			if tc.wantErr {
				failSummary := failureSummaryLog.String()
				if !strings.Contains(failSummary, wantAux) {
					t.Errorf("failure summary missing expected auxiliary outputs:\n%s\n\ngot:\n%s", wantAux, failSummary)
				}
			}
		})
	}
}
