// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/subcommands"
)

func TestCmd_Execute(t *testing.T) {
	const ninjaFileContent = `
rule cxx
  command = clang++ -c ${in} -o ${out}
  deps = gcc
build obj/foo.o: cxx foo.cc
`
	const dummySourceContent = `int main() { return 0; }`

	testCases := []struct {
		name  string
		args  []string
		setup func(t *testing.T, dir string)
		want  subcommands.ExitStatus
	}{
		{
			name: "valid target",
			args: []string{"-target", "obj/foo.o"},
			setup: func(t *testing.T, dir string) {
				writeFile := func(path, content string) {
					t.Helper()
					if err := os.WriteFile(path, []byte(content), 0644); err != nil {
						t.Fatalf("Failed to write %s: %v", path, err)
					}
				}
				writeFile(filepath.Join(dir, "build.ninja"), ninjaFileContent)
				writeFile(filepath.Join(dir, "foo.cc"), dummySourceContent)
				writeFile(filepath.Join(dir, ".siso_config"), "{}")
				writeFile(filepath.Join(dir, ".siso_filegroups"), "{}")
			},
			want: subcommands.ExitSuccess,
		},
		{
			name: "command line invocation",
			args: []string{"--", "clang++", "-c", "foo.cc", "-o", "obj/foo.o"},
			setup: func(t *testing.T, dir string) {
				writeFile := func(path, content string) {
					t.Helper()
					if err := os.WriteFile(path, []byte(content), 0644); err != nil {
						t.Fatalf("Failed to write %s: %v", path, err)
					}
				}
				writeFile(filepath.Join(dir, "foo.cc"), dummySourceContent)
				writeFile(filepath.Join(dir, ".siso_config"), "{}")
				writeFile(filepath.Join(dir, ".siso_filegroups"), "{}")
			},
			want: subcommands.ExitSuccess,
		},
		{
			name: "target not found -> unsupported compiler",
			args: []string{"-target", "obj/bar.o"},
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(ninjaFileContent), 0644); err != nil {
					t.Fatalf("Failed to write build.ninja: %v", err)
				}
			},
			want: subcommands.ExitFailure,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			tempDir, err := filepath.EvalSymlinks(tempDir)
			if err != nil {
				t.Fatalf("Failed to eval symlinks: %v", err)
			}
			err = os.MkdirAll(filepath.Join(tempDir, "build/config/siso"), 0755)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(tempDir, "out/siso")
			err = os.MkdirAll(filepath.Join(tempDir, "out/siso"), 0755)
			if err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, dir)
			}

			t.Chdir(tempDir)

			c := &Command{}
			flagSet := flag.NewFlagSet("test", flag.ContinueOnError)
			c.SetFlags(flagSet)

			if err := flagSet.Set("C", "out/siso"); err != nil {
				t.Fatalf("Failed to set -C flag: %v", err)
			}
			if err := flagSet.Parse(tc.args); err != nil {
				t.Fatalf("Failed to parse arguments: %v", err)
			}

			got := c.Execute(t.Context(), flagSet)

			if got != tc.want {
				t.Errorf("Cmd.Execute() returned exit code %v, want %v", got, tc.want)
			}
		})
	}
}
