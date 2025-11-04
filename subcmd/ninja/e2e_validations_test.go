// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_Validations(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"out"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d; want done=2 total=2; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d skipped=%d; want done=2 total=2 skipped=2; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
	outFI, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	validateFI, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Fatal(err)
	}

	t.Logf(`-- touch "in" only rebuilds "out"`)
	touchFile(t, dir, "in")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=1 skipped=1; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI2, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI2.ModTime().After(outFI.ModTime()) {
		t.Errorf("out should be updated. mtime diff=%s", outFI2.ModTime().Sub(outFI.ModTime()))
	}
	validateFI2, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI2.ModTime().Equal(validateFI.ModTime()) {
		t.Errorf("validate should not be updated. mtime diff=%s", validateFI2.ModTime().Sub(validateFI.ModTime()))
	}

	t.Logf(`-- touch "in2" only rebuilds "validate`)
	touchFile(t, dir, "in2")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=1 skipped=1; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI3, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI3.ModTime().Equal(outFI2.ModTime()) {
		t.Errorf("out should not be updated. mtime diff=%s", outFI3.ModTime().Sub(outFI2.ModTime()))
	}
	validateFI3, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI3.ModTime().After(validateFI2.ModTime()) {
		t.Errorf("validate should be updated. mtime diff=%s", validateFI3.ModTime().Sub(validateFI2.ModTime()))
	}
}

func TestBuild_ValidationsDependsOnOutput(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"out"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d; want done=2 total=2; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d skipped=%d; want done=2 total=2 skipped=2; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
	outFI, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	validateFI, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Fatal(err)
	}

	t.Logf(`-- touch "in" rebuilds "out" and "validate"`)
	touchFile(t, dir, "in")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 2 || stats.Skipped != 0 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=2 skipped=0; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI2, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI2.ModTime().After(outFI.ModTime()) {
		t.Errorf("out should be updated. mtime diff=%s", outFI2.ModTime().Sub(outFI.ModTime()))
	}
	validateFI2, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI2.ModTime().After(validateFI.ModTime()) {
		t.Errorf("validate should be updated. mtime diff=%s", validateFI2.ModTime().Sub(validateFI.ModTime()))
	}

	t.Logf(`-- touch "in2" only rebuilds "validate`)
	touchFile(t, dir, "in2")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=2 total=2 local=1 skipped=1; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI3, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI3.ModTime().Equal(outFI2.ModTime()) {
		t.Errorf("out should not be updated. mtime diff=%s", outFI3.ModTime().Sub(outFI2.ModTime()))
	}
	validateFI3, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI3.ModTime().After(validateFI2.ModTime()) {
		t.Errorf("validate should be updated. mtime diff=%s", validateFI3.ModTime().Sub(validateFI2.ModTime()))
	}
}

func TestBuild_ValidationsNested(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"out"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 3 {
		t.Errorf("done=%d total=%d; want done=3 total=3; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 3 {
		t.Errorf("done=%d total=%d skipped=%d; want done=3 total=3 skipped=3; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
	outFI, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	validateFI, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Fatal(err)
	}
	validateNestedFI, err := os.Stat(filepath.Join(dir, "out/siso/validate_nested"))
	if err != nil {
		t.Fatal(err)
	}

	t.Logf(`-- touch "in" only rebuilds "out"`)
	touchFile(t, dir, "in")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 2 || stats.Total != 3 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=3 total=3 local=1 skipped=2; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI2, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI2.ModTime().After(outFI.ModTime()) {
		t.Errorf("out should be updated. mtime diff=%s", outFI2.ModTime().Sub(outFI.ModTime()))
	}
	validateFI2, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI2.ModTime().Equal(validateFI.ModTime()) {
		t.Errorf("validate should not be updated. mtime diff=%s", validateFI2.ModTime().Sub(validateFI.ModTime()))
	}
	validateNestedFI2, err := os.Stat(filepath.Join(dir, "out/siso/validate_nested"))
	if err != nil {
		t.Errorf("validate_nested not found? %v", err)
	}
	if !validateNestedFI2.ModTime().Equal(validateNestedFI.ModTime()) {
		t.Errorf("validatel_nested should not be updated. mtime diff=%s", validateFI2.ModTime().Sub(validateFI.ModTime()))
	}

	t.Logf(`-- touch "in2" only rebuilds "validate`)
	touchFile(t, dir, "in2")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 2 || stats.Total != 3 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=3 total=3 local=1 skipped=2; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI3, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI3.ModTime().Equal(outFI2.ModTime()) {
		t.Errorf("out should not be updated. mtime diff=%s", outFI3.ModTime().Sub(outFI2.ModTime()))
	}
	validateFI3, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI3.ModTime().After(validateFI2.ModTime()) {
		t.Errorf("validate should be updated. mtime diff=%s", validateFI3.ModTime().Sub(validateFI2.ModTime()))
	}
	validateNestedFI3, err := os.Stat(filepath.Join(dir, "out/siso/validate_nested"))
	if err != nil {
		t.Errorf("validate_nested not found? %v", err)
	}
	if !validateNestedFI3.ModTime().Equal(validateNestedFI2.ModTime()) {
		t.Errorf("validate_nested should not be updated. mtime diff=%s", validateNestedFI3.ModTime().Sub(validateFI2.ModTime()))
	}

	t.Logf(`-- touch "in3" only rebuilds "validate_nested`)
	touchFile(t, dir, "in3")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 2 || stats.Total != 3 {
		t.Errorf("done=%d total=%d local=%d skipped=%d; want done=3 total=3 local=1 skipped=2; %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}
	outFI4, err := os.Stat(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("out not found? %v", err)
	}
	if !outFI4.ModTime().Equal(outFI3.ModTime()) {
		t.Errorf("out should not be updated. mtime diff=%s", outFI3.ModTime().Sub(outFI2.ModTime()))
	}
	validateFI4, err := os.Stat(filepath.Join(dir, "out/siso/validate"))
	if err != nil {
		t.Errorf("validate not found? %v", err)
	}
	if !validateFI4.ModTime().Equal(validateFI3.ModTime()) {
		t.Errorf("validate should not be updated. mtime diff=%s", validateFI4.ModTime().Sub(validateFI3.ModTime()))
	}
	validateNestedFI4, err := os.Stat(filepath.Join(dir, "out/siso/validate_nested"))
	if err != nil {
		t.Errorf("validate_nested not found? %v", err)
	}
	if !validateNestedFI4.ModTime().After(validateNestedFI3.ModTime()) {
		t.Errorf("validate_nested should be updated. mtime diff=%s", validateNestedFI4.ModTime().Sub(validateNestedFI3.ModTime()))
	}
}
