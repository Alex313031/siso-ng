// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestMatchInputRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching test is only on linux")
	}
	for _, tc := range []struct {
		name       string
		setup      func(*testing.T, string) []merkletree.Entry
		wantErr    bool
		wantInputs []string
	}{
		{
			// Symlink pointing to a directory when directory is expected (b/527756492 fix).
			name: "symlink_to_dir",
			setup: func(t *testing.T, dir string) []merkletree.Entry {
				t.Helper()
				// Local filesystem setup:
				// prebuilts/clang-tools/linux-x86/lib/clang/22/include/stddef.h
				// prebuilts/clang-tools/linux-x86/clang-headers -> lib/clang/22/include
				incDir := filepath.Join(dir, "prebuilts/clang-tools/linux-x86/lib/clang/22/include")
				err := os.MkdirAll(incDir, 0755)
				if err != nil {
					t.Fatal(err)
				}
				headerContent := []byte("// stddef.h content")
				err = os.WriteFile(filepath.Join(incDir, "stddef.h"), headerContent, 0644)
				if err != nil {
					t.Fatal(err)
				}

				symlinkPath := filepath.Join(dir, "prebuilts/clang-tools/linux-x86/clang-headers")
				err = os.Symlink("lib/clang/22/include", symlinkPath)
				if err != nil {
					t.Fatal(err)
				}
				return []merkletree.Entry{
					{
						Name: path.Path("prebuilts/clang-tools/linux-x86/clang-headers/stddef.h"),
						Data: blob.FromBytes(digest.SHA256, "stddef.h", headerContent),
					},
				}
			},
			wantInputs: []string{
				"prebuilts/clang-tools/linux-x86/clang-headers/stddef.h",
			},
		},
		{
			// Symlink pointing to a file when directory is expected.
			name: "symlink_to_file_when_dir_wanted",
			setup: func(t *testing.T, dir string) []merkletree.Entry {
				t.Helper()
				// Local filesystem setup:
				// prebuilts/clang-tools/linux-x86/somefile.txt
				// prebuilts/clang-tools/linux-x86/clang-headers -> somefile.txt
				baseDir := filepath.Join(dir, "prebuilts/clang-tools/linux-x86")
				err := os.MkdirAll(baseDir, 0755)
				if err != nil {
					t.Fatal(err)
				}
				err = os.WriteFile(filepath.Join(baseDir, "somefile.txt"), []byte("not a dir"), 0644)
				if err != nil {
					t.Fatal(err)
				}
				err = os.Symlink("somefile.txt", filepath.Join(baseDir, "clang-headers"))
				if err != nil {
					t.Fatal(err)
				}
				return []merkletree.Entry{
					{
						Name: path.Path("prebuilts/clang-tools/linux-x86/clang-headers/stddef.h"),
						Data: blob.FromBytes(digest.SHA256, "stddef.h", []byte("content")),
					},
				}
			},
			wantErr: true,
		},
		{
			// Regular directory matching directory.
			name: "regular_dir",
			setup: func(t *testing.T, dir string) []merkletree.Entry {
				t.Helper()
				// Local filesystem setup:
				headersDir := filepath.Join(dir, "prebuilts/clang-tools/linux-x86/clang-headers")
				err := os.MkdirAll(headersDir, 0755)
				if err != nil {
					t.Fatal(err)
				}
				headerContent := []byte("// stddef.h content")
				err = os.WriteFile(filepath.Join(headersDir, "stddef.h"), headerContent, 0644)
				if err != nil {
					t.Fatal(err)
				}
				return []merkletree.Entry{
					{
						Name: path.Path("prebuilts/clang-tools/linux-x86/clang-headers/stddef.h"),
						Data: blob.FromBytes(digest.SHA256, "stddef.h", headerContent),
					},
				}
			},
			wantInputs: []string{
				"prebuilts/clang-tools/linux-x86/clang-headers/stddef.h",
			},
		},
		// File digest mismatch.
		{
			name: "file_digest_mismatch",
			setup: func(t *testing.T, dir string) []merkletree.Entry {
				t.Helper()
				filePath := filepath.Join(dir, "foo.txt")
				err := os.WriteFile(filePath, []byte("local content"), 0644)
				if err != nil {
					t.Fatal(err)
				}
				return []merkletree.Entry{
					{
						Name: path.Path("foo.txt"),
						Data: blob.FromBytes(digest.SHA256, "foo.txt", []byte("remote content")),
					},
				}
			},
			wantErr: true,
		},
		{
			// Symlink target mismatch.
			name: "symlink_target_mismatch",
			setup: func(t *testing.T, dir string) []merkletree.Entry {
				t.Helper()
				symPath := filepath.Join(dir, "link.txt")
				err := os.Symlink("targetA", symPath)
				if err != nil {
					t.Fatal(err)
				}
				return []merkletree.Entry{
					{
						Name:   path.Path("link.txt"),
						Target: "targetB",
					},
				}
			},
			wantErr: true,
		},
		{
			name: "empty_dir",
			setup: func(t *testing.T, dir string) []merkletree.Entry {
				t.Helper()
				filePath := filepath.Join(dir, "bin/toybox")
				err := os.MkdirAll(filepath.Dir(filePath), 0755)
				if err != nil {
					t.Fatal(err)
				}
				err = os.WriteFile(filePath, []byte("toybox"), 0644)
				if err != nil {
					t.Fatal(err)
				}
				libDir := filepath.Join(dir, "lib64")
				err = os.MkdirAll(libDir, 0755)
				if err != nil {
					t.Fatal(err)
				}
				return []merkletree.Entry{
					{
						Name: path.Path("bin/toybox"),
						Data: blob.FromBytes(digest.SHA256, "bin/toybox", []byte("toybox")),
					},
					{
						Name: path.Path("lib64"),
					},
				}
			},
			wantInputs: []string{
				"bin/toybox",
				"lib64",
			},
		},
		{
			// Empty directories at several depths, next to a non-empty
			// directory that only holds an empty directory. Empty
			// directories come last, sorted.
			name: "nested_empty_dirs",
			setup: func(t *testing.T, dir string) []merkletree.Entry {
				t.Helper()
				for _, d := range []string{"z", "a/empty", "a/b/empty", "a/c"} {
					if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, "a/c/f"), []byte("f"), 0644); err != nil {
					t.Fatal(err)
				}
				return []merkletree.Entry{
					{Name: "z"},
					{Name: "a/empty"},
					{Name: "a/b/empty"},
					{
						Name: "a/c/f",
						Data: blob.FromBytes(digest.SHA256, "a/c/f", []byte("f")),
					},
				}
			},
			wantInputs: []string{
				"a/c/f",
				"a/b/empty",
				"a/empty",
				"z",
			},
		},
	} {
		ctx := t.Context()
		dir := t.TempDir()
		dir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}

		entries := tc.setup(t, dir)

		// Remote CAS setup:
		// prebuilts/clang-tools/linux-x86/clang-headers/stddef.h (clang-headers recorded as DirectoryNode)
		ds := blob.NewStore()
		tree := merkletree.New(digest.SHA256, ds)
		for _, ent := range entries {
			tree.Set(ent)
		}
		inputRootDigest, err := tree.Build(ctx)
		if err != nil {
			t.Fatal(err)
		}

		fakere := &reapitest.Fake{}
		reclient := reapitest.New(ctx, t, fakere)
		_, err = reclient.UploadAll(ctx, ds)
		if err != nil {
			t.Fatal(err)
		}

		hashFS, err := hashfs.New(ctx, hashfs.Option{})
		if err != nil {
			t.Fatal(err)
		}
		defer hashFS.Close(context.WithoutCancel(ctx))
		err = hashFS.WaitReady(ctx)
		if err != nil {
			t.Fatal(err)
		}

		b := &Builder{
			path:        NewPath(dir, "out/siso"),
			hashFS:      hashFS,
			reapiclient: reclient,
		}
		rt := reapiTwoPhaseCaching{b: b}

		// The second walk reuses what the first one cached on the
		// Builder, and must agree with it.
		for _, walk := range []string{"first", "second"} {
			inputs, err := rt.matchInputRoot(ctx, inputRootDigest)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("%s: %s matchInputRoot: %v; want %v", tc.name, walk, err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.wantInputs, inputs); diff != "" {
				t.Errorf("%s: %s matchInputRoot: inputs -want +got:\n%s", tc.name, walk, diff)
			}
		}
	}
}

// TestMatchActionLocalCommand checks that a candidate Action built from
// the same cmd (as siso builds and records it) matches with the local
// Command, without fetching the candidate's Command from CAS.
func TestMatchActionLocalCommand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching test is only on linux")
	}
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The input root has the working directory.
	if err := os.MkdirAll(filepath.Join(dir, "out/siso"), 0755); err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(context.WithoutCancel(ctx))
	if err := hashFS.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	cmd := &execute.Cmd{
		ID:            "test",
		Args:          []string{"touch", "foo.o"},
		Env:           []string{"B=2", "A=1"},
		WorkspaceRoot: dir,
		WorkDir:       "out/siso",
		Outputs:       []path.Path{"out/siso/foo.o"},
		Pure:          true,
		HashFS:        hashFS,
	}

	// The candidate: the Action that cmd.Digest builds, with everything
	// but its Command uploaded to CAS.
	ds := blob.NewStore()
	actionDigest, err := cmd.Digest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}
	actionData, ok := ds.Get(actionDigest)
	if !ok {
		t.Fatalf("action %s not in store", actionDigest)
	}
	buf, err := blob.DataToBytes(ctx, actionData)
	if err != nil {
		t.Fatal(err)
	}
	action := &rpb.Action{}
	if err := proto.Unmarshal(buf, action); err != nil {
		t.Fatal(err)
	}
	cmdDigest := digest.FromProto(action.GetCommandDigest())
	ds.Delete(cmdDigest)
	reclient := reapitest.New(ctx, t, &reapitest.Fake{})
	if _, err := reclient.UploadAll(ctx, ds); err != nil {
		t.Fatal(err)
	}
	rt := reapiTwoPhaseCaching{b: &Builder{
		path:        NewPath(dir, "out/siso"),
		hashFS:      hashFS,
		reapiclient: reclient,
	}}

	localDigest, err := cmd.CommandDigest(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if localDigest != cmdDigest {
		t.Fatalf("CommandDigest digest=%s; want the action's %s", localDigest, cmdDigest)
	}
	_, outputs, err := rt.matchAction(ctx, &Step{cmd: cmd}, action, localDigest)
	if err != nil {
		t.Fatalf("matchAction=%v; want nil", err)
	}
	if len(outputs) != 0 {
		t.Errorf("matchAction outputs=%q want empty", outputs)
	}

	// Without the local digest, the Command is fetched, and it is not in CAS.
	_, _, err = rt.matchAction(ctx, &Step{cmd: cmd}, action, digest.Digest{})
	if err == nil || !strings.Contains(err.Error(), "failed to fetch command") {
		t.Errorf("matchAction(other digest)=%v; want fetch error", err)
	}
}

// TestMatchInputRootAfterChange checks that a later walk sees a local change
// made through hashfs after an earlier walk matched, now that the earlier
// walk left everything loaded for hashfs.MatchDir.
func TestMatchInputRootAfterChange(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching test is only on linux")
	}
	for _, tc := range []struct {
		name    string
		change  func(context.Context, *hashfs.HashFS, string) error
		wantErr bool
	}{
		{
			name: "content",
			change: func(ctx context.Context, hfs *hashfs.HashFS, dir string) error {
				return hfs.WriteFile(ctx, dir, "a/b/g", []byte("new"), false, time.Now(), nil, nil)
			},
			wantErr: true,
		},
		{
			name: "executable",
			change: func(ctx context.Context, hfs *hashfs.HashFS, dir string) error {
				return hfs.WriteFile(ctx, dir, "a/b/g", []byte("g"), true, time.Now(), nil, nil)
			},
			wantErr: true,
		},
		{
			name: "remove_file",
			change: func(ctx context.Context, hfs *hashfs.HashFS, dir string) error {
				return hfs.Remove(ctx, dir, "a/f")
			},
			wantErr: true,
		},
		{
			name: "remove_dir",
			change: func(ctx context.Context, hfs *hashfs.HashFS, dir string) error {
				return hfs.RemoveAll(ctx, dir, "a/b")
			},
			wantErr: true,
		},
		{
			name: "dir_to_symlink",
			change: func(ctx context.Context, hfs *hashfs.HashFS, dir string) error {
				if err := hfs.RemoveAll(ctx, dir, "a"); err != nil {
					return err
				}
				return hfs.Symlink(ctx, dir, "y", "a", time.Now(), nil, nil)
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{"a/f": "f", "a/b/g": "g", "y/b/g": "y", "y/f": "f"} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
					t.Fatal(err)
				}
			}
			ds := blob.NewStore()
			tree := merkletree.New(digest.SHA256, ds)
			for name, data := range map[string]string{"a/f": "f", "a/b/g": "g"} {
				tree.Set(merkletree.Entry{Name: path.Path(name), Data: blob.FromBytes(digest.SHA256, name, []byte(data))})
			}
			inputRootDigest, err := tree.Build(ctx)
			if err != nil {
				t.Fatal(err)
			}
			reclient := reapitest.New(ctx, t, &reapitest.Fake{})
			if _, err := reclient.UploadAll(ctx, ds); err != nil {
				t.Fatal(err)
			}
			hashFS, err := hashfs.New(ctx, hashfs.Option{})
			if err != nil {
				t.Fatal(err)
			}
			defer hashFS.Close(context.WithoutCancel(ctx))
			if err := hashFS.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			rt := reapiTwoPhaseCaching{b: &Builder{
				path:        NewPath(dir, "out/siso"),
				hashFS:      hashFS,
				reapiclient: reclient,
			}}

			for _, walk := range []string{"first", "second"} {
				inputs, err := rt.matchInputRoot(ctx, inputRootDigest)
				if err != nil {
					t.Fatalf("%s matchInputRoot: %v; want nil", walk, err)
				}
				if diff := cmp.Diff([]string{"a/f", "a/b/g"}, inputs); diff != "" {
					t.Errorf("%s matchInputRoot: inputs -want +got:\n%s", walk, diff)
				}
			}
			if err := tc.change(ctx, hashFS, dir); err != nil {
				t.Fatal(err)
			}
			inputs, err := rt.matchInputRoot(ctx, inputRootDigest)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("matchInputRoot after change = %q, %v; want err %t", inputs, err, tc.wantErr)
			}
		})
	}
}

func TestMatchAction_EnvVars(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching test is only on linux")
	}
	for _, tc := range []struct {
		name      string
		cmdEnv    []string
		actionEnv []*rpb.Command_EnvironmentVariable
		wantErr   bool
	}{
		{
			name: "both_empty",
		},
		{
			name:   "matching_env",
			cmdEnv: []string{"B=2", "A=1"},
			actionEnv: []*rpb.Command_EnvironmentVariable{
				{Name: "A", Value: "1"},
				{Name: "B", Value: "2"},
			},
		},
		{
			name:   "mismatch_value",
			cmdEnv: []string{"A=1", "B=2"},
			actionEnv: []*rpb.Command_EnvironmentVariable{
				{Name: "A", Value: "1"},
				{Name: "B", Value: "different"},
			},
			wantErr: true,
		},
		{
			name:    "missing_in_action",
			cmdEnv:  []string{"A=1"},
			wantErr: true,
		},
		{
			name: "extra_in_action",
			actionEnv: []*rpb.Command_EnvironmentVariable{
				{Name: "A", Value: "1"},
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			dir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}

			ds := blob.NewStore()
			tree := merkletree.New(digest.SHA256, ds)
			inputRootDigest, err := tree.Build(ctx)
			if err != nil {
				t.Fatal(err)
			}

			cmdProto := &rpb.Command{
				Arguments:            []string{"clang", "-c", "foo.c"},
				WorkingDirectory:     "out/siso",
				EnvironmentVariables: tc.actionEnv,
				OutputPaths:          []string{"foo.o"},
			}
			cmdData, err := blob.FromProtoMessage(digest.SHA256, cmdProto)
			if err != nil {
				t.Fatal(err)
			}
			ds.Set(cmdData)

			actionProto := &rpb.Action{
				CommandDigest:   cmdData.Digest().Proto(),
				InputRootDigest: inputRootDigest.Proto(),
			}

			fakere := &reapitest.Fake{}
			reclient := reapitest.New(ctx, t, fakere)
			_, err = reclient.UploadAll(ctx, ds)
			if err != nil {
				t.Fatal(err)
			}

			hashFS, err := hashfs.New(ctx, hashfs.Option{})
			if err != nil {
				t.Fatal(err)
			}
			defer hashFS.Close(context.WithoutCancel(ctx))
			if err := hashFS.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}

			b := &Builder{
				path:        NewPath(dir, "out/siso"),
				hashFS:      hashFS,
				reapiclient: reclient,
			}
			rt := reapiTwoPhaseCaching{b: b}
			step := &Step{
				cmd: &execute.Cmd{
					Args:    []string{"clang", "-c", "foo.c"},
					WorkDir: "out/siso",
					Env:     tc.cmdEnv,
				},
			}

			_, _, err = rt.matchAction(ctx, step, actionProto, digest.Digest{})
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("matchAction() err = %v; wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestMatchAction_WorkDir(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cmdDir    path.Path
		actionDir string
		wantErr   bool
	}{
		{
			name:      "both_dot",
			cmdDir:    ".",
			actionDir: ".",
		},
		{
			name:      "both_empty",
			cmdDir:    "",
			actionDir: "",
		},
		{
			name:      "cmd_dot_action_empty",
			cmdDir:    ".",
			actionDir: "",
		},
		{
			name:      "cmd_empty_action_dot",
			cmdDir:    "",
			actionDir: ".",
		},
		{
			name:      "same_subdir",
			cmdDir:    "out/siso",
			actionDir: "out/siso",
		},
		{
			name:      "different_subdir",
			cmdDir:    "out/siso",
			actionDir: "out/other",
			wantErr:   true,
		},
		{
			name:      "dot_vs_subdir",
			cmdDir:    ".",
			actionDir: "out/siso",
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			dir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}

			ds := blob.NewStore()
			tree := merkletree.New(digest.SHA256, ds)
			inputRootDigest, err := tree.Build(ctx)
			if err != nil {
				t.Fatal(err)
			}

			cmdProto := &rpb.Command{
				Arguments:        []string{"clang", "-c", "foo.c"},
				WorkingDirectory: tc.actionDir,
				OutputPaths:      []string{"foo.o"},
			}
			cmdData, err := blob.FromProtoMessage(digest.SHA256, cmdProto)
			if err != nil {
				t.Fatal(err)
			}
			ds.Set(cmdData)

			actionProto := &rpb.Action{
				CommandDigest:   cmdData.Digest().Proto(),
				InputRootDigest: inputRootDigest.Proto(),
			}

			fakere := &reapitest.Fake{}
			reclient := reapitest.New(ctx, t, fakere)
			_, err = reclient.UploadAll(ctx, ds)
			if err != nil {
				t.Fatal(err)
			}

			hashFS, err := hashfs.New(ctx, hashfs.Option{})
			if err != nil {
				t.Fatal(err)
			}
			defer hashFS.Close(context.WithoutCancel(ctx))
			if err := hashFS.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}

			b := &Builder{
				path:        NewPath(dir, "out/siso"),
				hashFS:      hashFS,
				reapiclient: reclient,
			}
			rt := reapiTwoPhaseCaching{b: b}
			step := &Step{
				cmd: &execute.Cmd{
					Args:    []string{"clang", "-c", "foo.c"},
					WorkDir: tc.cmdDir,
				},
			}

			_, _, err = rt.matchAction(ctx, step, actionProto, digest.Digest{})
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("matchAction() err = %v; wantErr %t", err, tc.wantErr)
			}
		})
	}
}
