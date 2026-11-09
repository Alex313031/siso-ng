// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import "testing"

func TestIsDirTarget(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"obj/extracted/", true},
		{"out/siso/obj/extracted/", true},
		{"/absolute/path/dir/", true},
		{"obj/file.o", false},
		{"out/siso/output.zip", false},
		{"", false},
		{"/", true},
	}
	for _, tt := range tests {
		got := IsDirTarget(tt.path)
		if got != tt.want {
			t.Errorf("IsDirTarget(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestDirTargetPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"obj/extracted/", "obj/extracted"},
		{"out/siso/obj/extracted/", "out/siso/obj/extracted"},
		{"obj/file.o", "obj/file.o"},
		{"", ""},
	}
	for _, tt := range tests {
		got := DirTargetPath(tt.path)
		if got != tt.want {
			t.Errorf("DirTargetPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
