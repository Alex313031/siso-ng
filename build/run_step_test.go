// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestRunStep_RestatContent_CacheHit(t *testing.T) {
	ctx := t.Context()
	fakere := &reapitest.Fake{}
	env := newRacingTestEnv(t, fakere)

	content := []byte("output content")
	outRel := "out/siso/out.txt"
	fullOut := filepath.Join(env.dir, outRel)
	if err := os.MkdirAll(filepath.Dir(fullOut), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullOut, content, 0644); err != nil {
		t.Fatal(err)
	}

	data := blob.FromBytes(digest.SHA256, outRel, content)
	d := data.Digest()
	oldTime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	err := env.hashFS.Update(ctx, env.dir, []hashfs.UpdateEntry{{
		Name: path.Path(outRel),
		Entry: &merkletree.Entry{
			Name: path.Path(outRel),
			Data: data,
		},
		IsLocal:     true,
		Mode:        0644,
		ModTime:     oldTime,
		UpdatedTime: oldTime,
		CmdHash:     []byte("old-cmdhash"),
		EdgeHash:    []byte("old-edgehash"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fakere.Put(ctx, content); err != nil {
		t.Fatal(err)
	}

	cache, err := NewCache(ctx, CacheOptions{
		Store:      env.cachestore,
		EnableRead: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	b := env.newBuilder(t, func(opts *Options) {
		opts.Cache = cache
		opts.RECacheEnableRead = true
	})
	b.plan = &plan{closed: true}
	b.stats = newStats(1)

	stepDef := fakeStepDef{
		actionName: "gen",
		command:    "touch out.txt",
		outputs:    []path.Path{path.Path(outRel)},
		pure:       true,
		platform:   map[string]string{"container-image": "docker://ubuntu"},
		bindings: map[string]string{
			"restat":         "1",
			"restat_content": "true",
		},
	}

	// Pre-populate the action cache with the digest that step.init will compute.
	manifest := b.newStepManifest(ctx, stepDef)
	seedCmd := newCmd(ctx, b, stepDef, manifest)
	ds := blob.NewStore()
	actionDigest, err := seedCmd.Digest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.reclient.UploadAll(ctx, ds); err != nil {
		t.Fatal(err)
	}
	err = env.cachestore.SetActionResult(ctx, actionDigest, &rpb.ActionResult{
		OutputFiles: []*rpb.OutputFile{
			{
				Path:   "out.txt",
				Digest: d.Proto(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	step := &Step{
		state: &stepState{},
		def:   stepDef,
	}
	if err := b.runStep(ctx, step); err != nil {
		t.Fatalf("runStep(ctx, step)=%v; want nil", err)
	}
	if !step.metrics.Cached {
		t.Fatalf("step.metrics.Cached=%t; want true", step.metrics.Cached)
	}

	fi, err := env.hashFS.Stat(ctx, env.dir, path.Path(outRel))
	if err != nil {
		t.Fatalf("Stat(ctx, %q, %q)=%v; want nil", env.dir, outRel, err)
	}
	if fi.IsChanged() {
		t.Errorf("fi.IsChanged()=%t; want false", fi.IsChanged())
	}
	if !fi.ModTime().Equal(oldTime) {
		t.Errorf("fi.ModTime()=%v; want %v", fi.ModTime(), oldTime)
	}
	if !fi.UpdatedTime().After(oldTime) {
		t.Errorf("fi.UpdatedTime()=%v; want > %v", fi.UpdatedTime(), oldTime)
	}
}
