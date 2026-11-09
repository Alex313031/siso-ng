// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package nsjailutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRenameFromJail verifies renameFromJail captures an output out of the nsjail exec root, replacing a non-empty pre-created destination cleanly (a plain os.Rename of a dir onto a non-empty dir fails ENOTEMPTY).
func TestRenameFromJail(t *testing.T) {
	t.Run("file_overwrites_destination", func(t *testing.T) {
		dir := t.TempDir()
		jail := filepath.Join(dir, "jail")
		dst := filepath.Join(dir, "dst")
		if err := os.MkdirAll(jail, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(jail, "out.txt"), []byte("NEW"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dst, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, "out.txt"), []byte("OLD"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := renameFromJail(filepath.Join(jail, "out.txt"), filepath.Join(dst, "out.txt")); err != nil {
			t.Fatalf("renameFromJail(file): %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dst, "out.txt"))
		if err != nil || string(got) != "NEW" {
			t.Fatalf("dst content = %q, %v; want NEW", got, err)
		}
	})

	t.Run("dir_onto_populated_destination", func(t *testing.T) {
		dir := t.TempDir()
		jail := filepath.Join(dir, "jail", "gen")
		dst := filepath.Join(dir, "out", "gen")
		if err := os.MkdirAll(jail, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(jail, "new.txt"), []byte("NEW"), 0644); err != nil {
			t.Fatal(err)
		}
		// Destination pre-created and populated by a prior build.
		if err := os.MkdirAll(dst, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, "stale.txt"), []byte("OLD"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := renameFromJail(jail, dst); err != nil {
			t.Fatalf("renameFromJail(dir onto populated dir): %v", err)
		}
		if _, err := os.Stat(filepath.Join(dst, "new.txt")); err != nil {
			t.Errorf("captured dir missing new.txt: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
			t.Errorf("stale file from previous build survived capture: err=%v", err)
		}
	})
}

func TestNew_PublicAndWritableDirs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("NSJail is only supported on linux")
	}
	t.Setenv("SISO_NSJAIL_PUBLIC_DIRS", "")
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	jailRoot := filepath.Join(dir, "jailroot")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jailRoot, 0755); err != nil {
		t.Fatal(err)
	}

	t.Run("default_mounts_when_unset", func(t *testing.T) {
		req := Request{
			ExePath:       "/bin/true",
			JailRootDir:   jailRoot,
			WorkspaceRoot: ws,
			WorkDir:       "out",
			OutDir:        "out",
		}
		jail, err := New(t.Context(), os.DirFS("/"), req)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer jail.Close()

		foundBin := false
		for _, m := range jail.config.Mount {
			if m.GetDst() == "/bin" && !m.GetRw() {
				foundBin = true
			}
		}
		if !foundBin {
			t.Errorf("expected /bin read-only mount by default, got mounts: %v", jail.config.Mount)
		}
	})

	t.Run("custom_public_and_writable_dirs", func(t *testing.T) {
		req := Request{
			ExePath:       "/bin/true",
			JailRootDir:   jailRoot,
			WorkspaceRoot: ws,
			WorkDir:       "out",
			OutDir:        "out",
			PublicDirs:    []string{"/custom/ro"},
			WritableDirs:  []string{"/custom/rw"},
		}
		jail, err := New(t.Context(), os.DirFS("/"), req)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer jail.Close()

		var foundRO, foundRW, foundDefaultBin bool
		for _, m := range jail.config.Mount {
			switch m.GetDst() {
			case "/custom/ro":
				foundRO = !m.GetRw()
			case "/custom/rw":
				foundRW = m.GetRw()
			case "/bin":
				foundDefaultBin = true
			}
		}
		if !foundRO {
			t.Errorf("expected /custom/ro mount with rw=false, got mounts: %v", jail.config.Mount)
		}
		if !foundRW {
			t.Errorf("expected /custom/rw mount with rw=true, got mounts: %v", jail.config.Mount)
		}
		if foundDefaultBin {
			t.Errorf("did not expect default /bin mount when custom dirs provided, got mounts: %v", jail.config.Mount)
		}
	})
}
