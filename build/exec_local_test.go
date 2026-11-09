// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// TestIsRequiredOutput verifies directory outputs are classified as required even though def.Outputs keeps trailing slashes while cmd.AllOutputs strips them.
func TestIsRequiredOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		out        string
		defOutputs []string
		want       bool
	}{
		{
			name:       "file_output_required",
			out:        "obj/foo.o",
			defOutputs: []string{"obj/foo.o", "obj/bar.o"},
			want:       true,
		},
		{
			name:       "file_output_not_in_def",
			out:        "obj/baz.o",
			defOutputs: []string{"obj/foo.o", "obj/bar.o"},
			want:       false,
		},
		{
			name:       "dir_output_required_trailing_slash_in_def",
			out:        "obj/extracted",
			defOutputs: []string{"obj/extracted/"},
			want:       true,
		},
		{
			name:       "mixed_file_and_dir_outputs",
			out:        "obj/extracted",
			defOutputs: []string{"obj/repackaged.zip", "obj/extracted/"},
			want:       true,
		},
		{
			name:       "dir_output_not_in_def",
			out:        "obj/other",
			defOutputs: []string{"obj/extracted/"},
			want:       false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := isRequiredOutput(path.Path(tc.out), path.Paths(tc.defOutputs))
			if got != tc.want {
				t.Errorf("isRequiredOutput(%q, %v) = %v; want %v",
					tc.out, tc.defOutputs, got, tc.want)
			}
		})
	}
}

// TestCheckLocalOutputs_UpdatesTargets verifies that checkLocalOutputs updates
// b.targets so that a pre-existing stale targetState entry (populated when an
// earlier step with a stale .siso_deps entry stat'd the file before the local
// generator ran) is overwritten with the new mtime and changed=true.
func TestCheckLocalOutputs_UpdatesTargets(t *testing.T) {
	ctx := t.Context()
	env := newRacingTestEnv(t, &reapitest.Fake{})
	b := env.newBuilder(t, nil)

	outRel := path.Path("out/siso/gen/header.h")
	fullOut := filepath.Join(env.dir, string(outRel))
	if err := os.MkdirAll(filepath.Dir(fullOut), 0755); err != nil {
		t.Fatal(err)
	}
	oldContent := []byte("// v1\n")
	if err := os.WriteFile(fullOut, oldContent, 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	err := env.hashFS.Update(ctx, env.dir, []hashfs.UpdateEntry{{
		Name: outRel,
		Entry: &merkletree.Entry{
			Name: outRel,
			Data: blob.FromBytes(digest.SHA256, string(outRel), oldContent),
		},
		IsLocal:     true,
		IsChanged:   false,
		Mode:        0644,
		ModTime:     oldTime,
		UpdatedTime: oldTime,
		CmdHash:     []byte("old-cmdhash"),
		EdgeHash:    []byte("old-edgehash"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an earlier step whose stale .siso_deps references outRel before
	// the local generator step runs. This caches {mtime: oldTime, changed: false}
	// in b.targets.
	consumerDef := fakeStepDef{
		actionName:    "cxx",
		triggerInputs: []path.Path{outRel},
	}
	_, mtime, err := inputMtime(ctx, b, consumerDef)
	if err != nil {
		t.Fatalf("initial inputMtime() err=%v; want nil", err)
	}
	if !mtime.Equal(oldTime) {
		t.Fatalf("initial inputMtime() mtime=%v; want %v", mtime, oldTime)
	}

	// Now simulate the local generator step updating outRel in HashFS and
	// running checkLocalOutputs.
	newContent := []byte("// v2\n")
	if err := os.WriteFile(fullOut, newContent, 0644); err != nil {
		t.Fatal(err)
	}
	newTime := oldTime.Add(10 * time.Minute)
	err = env.hashFS.Update(ctx, env.dir, []hashfs.UpdateEntry{{
		Name: outRel,
		Entry: &merkletree.Entry{
			Name: outRel,
			Data: blob.FromBytes(digest.SHA256, string(outRel), newContent),
		},
		IsLocal:     true,
		IsChanged:   true,
		Mode:        0644,
		ModTime:     newTime,
		UpdatedTime: newTime,
		CmdHash:     []byte("new-cmdhash"),
		EdgeHash:    []byte("new-edgehash"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	dirRel := path.Path("out/siso/gen/dir_out")
	dirMemberRel := path.Path("out/siso/gen/dir_out/nested.h")
	fullDirMember := filepath.Join(env.dir, string(dirMemberRel))
	if err := os.MkdirAll(filepath.Dir(fullDirMember), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullDirMember, newContent, 0644); err != nil {
		t.Fatal(err)
	}
	dirMemberTime := newTime.Add(5 * time.Minute)
	err = env.hashFS.Update(ctx, env.dir, []hashfs.UpdateEntry{
		{
			Name:        dirRel,
			Entry:       &merkletree.Entry{Name: dirRel},
			IsLocal:     true,
			IsChanged:   true,
			Mode:        fs.ModeDir | 0755,
			ModTime:     oldTime,
			UpdatedTime: oldTime,
			CmdHash:     []byte("new-cmdhash"),
			EdgeHash:    []byte("new-edgehash"),
		},
		{
			Name: dirMemberRel,
			Entry: &merkletree.Entry{
				Name: dirMemberRel,
				Data: blob.FromBytes(digest.SHA256, string(dirMemberRel), newContent),
			},
			IsLocal:     true,
			IsChanged:   true,
			Mode:        0644,
			ModTime:     dirMemberTime,
			UpdatedTime: dirMemberTime,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	genCmd := &execute.Cmd{
		HashFS:        env.hashFS,
		WorkspaceRoot: env.dir,
		Outputs:       []path.Path{outRel},
		OutputDirs:    []path.Path{dirRel},
	}
	genCmd.SetActionResult(&rpb.ActionResult{ExitCode: 0}, false)
	genStep := newRacingTestStep(genCmd, []string{string(outRel), string(dirRel) + "/"})
	if err := b.checkLocalOutputs(ctx, genStep); err != nil {
		t.Fatalf("checkLocalOutputs()=%v; want nil", err)
	}

	// Downstream step depending on outRel must now see errDirty and the new mtime.
	_, _, err = inputMtime(ctx, b, consumerDef)
	if !errors.Is(err, errDirty) {
		t.Errorf("inputMtime() after checkLocalOutputs err=%v; want %v", err, errDirty)
	}
	ts, ok := b.targets.Load(string(outRel))
	if !ok {
		t.Fatalf("b.targets.Load(%q) not found", outRel)
	}
	if !ts.changed || !ts.mtime.Equal(newTime) || ts.dirtyErr != nil {
		t.Errorf("b.targets.Load(%q)=%+v; want mtime=%v changed=true dirtyErr=nil", outRel, ts, newTime)
	}
	dirTS, ok := b.targets.Load(string(dirRel))
	if !ok {
		t.Fatalf("b.targets.Load(%q) not found", dirRel)
	}
	if !dirTS.changed || !dirTS.mtime.Equal(dirMemberTime) || dirTS.dirtyErr != nil {
		t.Errorf("b.targets.Load(%q)=%+v; want effective mtime=%v changed=true dirtyErr=nil", dirRel, dirTS, dirMemberTime)
	}
}

// TestCheckLocalOutputs_DirWalkErrorStoresDirtyErr verifies that when walking a
// directory output fails (walkErr != nil), both checkLocalOutputs and outputs
// store dirtyErr in b.targets rather than caching the fallback mtime.
func TestCheckLocalOutputs_DirWalkErrorStoresDirtyErr(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required bool
		run      func(b *Builder, step *Step) error
	}{
		{
			name:     "checkLocalOutputs_optional",
			required: false,
			run: func(b *Builder, step *Step) error {
				return b.checkLocalOutputs(t.Context(), step)
			},
		},
		{
			name:     "checkLocalOutputs_required",
			required: true,
			run: func(b *Builder, step *Step) error {
				return b.checkLocalOutputs(t.Context(), step)
			},
		},
		{
			name:     "outputs_optional",
			required: false,
			run: func(b *Builder, step *Step) error {
				return b.outputs(t.Context(), step)
			},
		},
		{
			name:     "outputs_required",
			required: true,
			run: func(b *Builder, step *Step) error {
				return b.outputs(t.Context(), step)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			env := newRacingTestEnv(t, &reapitest.Fake{})
			b := env.newBuilder(t, nil)

			dirOut := path.Path("out/siso/gen/dir_out")
			if err := os.MkdirAll(filepath.Dir(filepath.Join(env.dir, string(dirOut))), 0755); err != nil {
				t.Fatal(err)
			}
			// Seed b.targets with a clean pre-existing entry to ensure it gets
			// overwritten with dirtyErr when walking the directory fails.
			b.targets.Store(string(dirOut), targetState{
				mtime:   time.Now().Add(-1 * time.Hour),
				changed: false,
			})

			// Record dirOut in HashFS as a dangling symlink: HashFS.Stat succeeds
			// (returning the symlink entry itself), while FileSystem's WalkDir
			// follows the symlink and fails with fs.ErrNotExist.
			err := env.hashFS.Symlink(ctx, env.dir, "nonexistent_target", dirOut, time.Now(), []byte("cmdhash"), []byte("edgehash"))
			if err != nil {
				t.Fatal(err)
			}

			var defOutputs []path.Path
			if tc.required {
				defOutputs = []path.Path{dirOut + "/"}
			}
			cmd := &execute.Cmd{
				HashFS:        env.hashFS,
				WorkspaceRoot: env.dir,
				OutputDirs:    []path.Path{dirOut},
			}
			cmd.SetActionResult(&rpb.ActionResult{ExitCode: 0}, false)
			step := &Step{
				cmd:            cmd,
				state:          &stepState{},
				startReported:  new(sync.Once),
				finishReported: new(sync.Once),
				def: fakeStepDef{
					actionName: "gen_dir",
					outputs:    defOutputs,
				},
			}

			err = tc.run(b, step)
			if tc.required {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("err=%v; want %v", err, fs.ErrNotExist)
				}
			} else if err != nil {
				t.Errorf("err=%v; want nil", err)
			}

			ts, ok := b.targets.Load(string(dirOut))
			if !ok {
				t.Fatalf("b.targets.Load(%q) not found", dirOut)
			}
			if !errors.Is(ts.dirtyErr, fs.ErrNotExist) {
				t.Errorf("b.targets.Load(%q).dirtyErr=%v; want %v (full state: %+v)", dirOut, ts.dirtyErr, fs.ErrNotExist, ts)
			}
		})
	}
}
