// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package merkletree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/reapi/digest"
)

func TestSet(t *testing.T) {
	for _, tc := range []struct {
		Entry
		wantErr  bool
		wantName string
		wantNode proto.Message // one of *rpb.FileNode or *rpb.SymlinkNode
		wantDirs []string
	}{
		{
			Entry: Entry{
				Name:         "third_party/llvm-build/Release+Asserts/bin/clang",
				Data:         digest.FromBytes("clang binary", []byte("clang binary")),
				IsExecutable: true,
			},
			wantName: "clang",
			wantNode: &rpb.FileNode{
				Name: "third_party/llvm-build/Release+Asserts/bin/clang",
			},
			wantDirs: []string{
				"",
				"third_party",
				"third_party/llvm-build",
				"third_party/llvm-build/Release+Asserts",
				"third_party/llvm-build/Release+Asserts/bin",
			},
		},
		{
			Entry: Entry{
				Name:   "third_party/llvm-build/Release+Asserts/bin/clang++",
				Target: "clang",
			},
			wantName: "clang++",
			wantNode: &rpb.SymlinkNode{
				Name:   "third_party/llvm-build/Release+Asserts/bin/clang++",
				Target: "clang",
			},
			wantDirs: []string{
				"",
				"third_party",
				"third_party/llvm-build",
				"third_party/llvm-build/Release+Asserts",
				"third_party/llvm-build/Release+Asserts/bin",
			},
		},
		{
			Entry: Entry{
				Name: "path/../name",
			},
			// create 'path' dir and 'name' dir.
			wantDirs: []string{
				"",
				"path",
				"name",
			},
		},
		{
			Entry: Entry{
				Name: "../path/name",
			},
			// out of root.
			wantErr: true,
		},
		{
			Entry: Entry{
				Name: "path/name/..",
			},
			// create 'path/name' dir.
			wantDirs: []string{
				"",
				"path",
				"path/name",
			},
		},
		{
			Entry: Entry{
				Name: "path/name/.",
			},
			wantDirs: []string{
				"",
				"path",
				"path/name",
			},
		},
		{
			Entry: Entry{
				Name: "path/./name",
			},
			wantDirs: []string{
				"",
				"path",
				"path/name",
			},
		},
		{
			Entry: Entry{
				Name: "path//name",
			},
			wantDirs: []string{
				"",
				"path",
				"path/name",
			},
		},
		{
			Entry: Entry{
				Name: "..",
			},
			// out of root.
			wantErr: true,
		},
		{
			Entry: Entry{
				Name: "path/name/.",
				Data: digest.FromBytes("file", []byte("file")),
			},
			wantErr: true,
		},
		{
			Entry: Entry{
				Name: "path/name/..",
				Data: digest.FromBytes("file", []byte("file")),
			},
			wantErr: true,
		},
		{
			Entry: Entry{
				Name: "path/name/../../foo",
			},
			// "foo" dir
			wantDirs: []string{
				"",
				"path",
				"path/name",
				"foo",
			},
		},
		{
			Entry: Entry{
				Name: "path/name/../../foo",
				Data: digest.FromBytes("file", []byte("file")),
			},
			// "foo" file
			wantName: "foo",
			wantNode: &rpb.FileNode{
				Name: "foo",
			},
			wantDirs: []string{
				"",
				"path",
				"path/name",
			},
		},
		{
			Entry: Entry{
				Name: "path/name/../..",
			},
			// root dir
			wantDirs: []string{
				"",
				"path",
				"path/name",
			},
		},
		{
			Entry: Entry{
				Name: "path/name/../..",
				Data: digest.FromBytes("file", []byte("file")),
			},
			// .. should not be file.
			wantErr: true,
		},
		{
			Entry: Entry{
				Name: "path/name/../../../path/foo",
			},
			// go outside of root.
			wantErr: true,
		},
		{
			Entry: Entry{
				Name: "/full/path/name",
			},
			wantErr: true,
		},
	} {
		ds := digest.NewStore()
		mt := New(ds)
		err := mt.Set(tc.Entry)
		if (err != nil) != tc.wantErr {
			t.Errorf("mt.Set(%v)=%v; want err=%t", tc.Entry, err, tc.wantErr)
		}
		if tc.wantErr {
			continue
		}
		t.Logf("check for mt.Set(%v)", tc.Entry)
		if tc.wantNode != nil {
			key := ""
			switch wantNode := tc.wantNode.(type) {
			case *rpb.FileNode:
				key = wantNode.Name
			case *rpb.SymlinkNode:
				key = wantNode.Name
			default:
				t.Fatalf("Wrong node type: %T", tc.wantNode)
			}
			node, err := getNode(mt, key)
			if err != nil {
				t.Errorf("node(%q)=%#v, %v; want node, nil", key, node, err)
			}
		}
		for _, dirname := range tc.wantDirs {
			node, err := getNode(mt, dirname)
			_, ok := node.(*rpb.Directory)
			if err != nil || !ok {
				t.Errorf("node(%q)=%#v, %v; want directory, nil", dirname, node, err)
			}
		}
		sort.Strings(tc.wantDirs)
		var dirs []string
		for k := range mt.m {
			dirs = append(dirs, k)
		}
		sort.Strings(dirs)
		if !cmp.Equal(dirs, tc.wantDirs) {
			t.Errorf("dirs=%q; want=%q", dirs, tc.wantDirs)
		}
	}
}

func getNode(mt *MerkleTree, path string) (proto.Message, error) {
	elems := strings.Split(filepath.Clean(path), "/")
	cur := mt.m[""]
	if len(elems) == 0 {
		return cur, nil
	}
	var paths []string
	for {
		var name string
		name, elems = elems[0], elems[1:]
		if len(elems) == 0 {
			for _, n := range cur.Files {
				if name == n.Name {
					return n, nil
				}
			}
			for _, n := range cur.Symlinks {
				if name == n.Name {
					return n, nil
				}
			}
			return cur, nil
		}
		paths = append(paths, name)
		var ok bool
		cur, ok = mt.m[strings.Join(paths, "/")]
		if !ok {
			return nil, fmt.Errorf("%s not found in %s", name, strings.Join(paths[:len(paths)-1], "/"))
		}
		if cur == nil {
			return nil, fmt.Errorf("%s in subtree %s", name, strings.Join(paths[:len(paths)-1], "/"))
		}
	}
}

func TestBuildInvalidEntry(t *testing.T) {
	ds := digest.NewStore()
	mt := New(ds)

	for _, ent := range []Entry{
		{
			// Invalid Entry: Absolute path.
			Name:         "/usr/bin/third_party/llvm-build/Release+Asserts/bin/clang",
			Data:         digest.FromBytes("clang binary", []byte("clang binary")),
			IsExecutable: true,
		},
		{
			// Invalid Entry: has both `Data` and `Target` fields set.
			Name:   "third_party/llvm-build/Release+Asserts/bin/clang++",
			Data:   digest.FromBytes("clang binary", []byte("clang binary")),
			Target: "clang",
		},
	} {
		err := mt.Set(ent)
		if err == nil {
			t.Fatalf("mt.Set(%q)=nil; want=(error)", ent.Name)
		}
	}
}

func TestBuild(t *testing.T) {
	ctx := t.Context()

	ds := digest.NewStore()
	mt := New(ds)

	for _, ent := range []Entry{
		{
			Name:         "third_party/llvm-build/Release+Asserts/bin/clang",
			Data:         digest.FromBytes("clang binary", []byte("clang binary")),
			IsExecutable: true,
		},
		{
			Name:   "third_party/llvm-build/Release+Asserts/bin/clang++-1",
			Target: "clang",
		},
		{
			Name:   "third_party/llvm-build/Release+Asserts/bin/clang++",
			Target: "clang",
		},
		{
			Name: "base/build_time.h",
			Data: digest.FromBytes("base_time.h", []byte("byte_time.h content")),
		},
		{
			Name: "out/Release/obj/base",
			// directory
		},
		{
			Name: "base/debug/debugger.cc",
			Data: digest.FromBytes("debugger.cc", []byte("debugger.cc content")),
		},
		{
			Name: "base/test/../macros.h",
			Data: digest.FromBytes("macros.h", []byte("macros.h content")),
		},
		// de-dup for same content http://b/124693412
		{
			Name: "third_party/skia/include/private/SkSafe32.h",
			Data: digest.FromBytes("SkSafe32.h", []byte("SkSafe32.h content")),
		},
		{
			Name: "third_party/skia/include/private/SkSafe32.h",
			Data: digest.FromBytes("SkSafe32.h", []byte("SkSafe32.h content")),
		},
	} {
		err := mt.Set(ent)
		if err != nil {
			t.Fatalf("mt.Set(%q)=%v; want=nil", ent.Name, err)
		}
	}

	d, err := mt.Build(ctx)
	if err != nil {
		t.Fatalf("mt.Build()=_, %v; want=nil", err)
	}

	dir, err := openDir(ctx, ds, d)
	if err != nil {
		t.Fatalf("root %v not found: %v", d, err)
	}
	checkDir(ctx, t, ds, dir, "",
		nil,
		[]string{"base", "out", "third_party"},
		nil)
	baseDir := checkDir(ctx, t, ds, dir, "base",
		[]string{"build_time.h", "macros.h"},
		[]string{"debug", "test"},
		nil)

	checkDir(ctx, t, ds, baseDir, "debug",
		[]string{"debugger.cc"},
		nil, nil)
	checkDir(ctx, t, ds, baseDir, "test",
		nil, nil, nil)

	outDir := checkDir(ctx, t, ds, dir, "out", nil, []string{"Release"}, nil)
	releaseDir := checkDir(ctx, t, ds, outDir, "Release", nil, []string{"obj"}, nil)
	objDir := checkDir(ctx, t, ds, releaseDir, "obj", nil, []string{"base"}, nil)
	checkDir(ctx, t, ds, objDir, "base", nil, nil, nil)

	tpDir := checkDir(ctx, t, ds, dir, "third_party", nil, []string{"llvm-build", "skia"}, nil)
	llvmDir := checkDir(ctx, t, ds, tpDir, "llvm-build", nil, []string{"Release+Asserts"}, nil)
	raDir := checkDir(ctx, t, ds, llvmDir, "Release+Asserts", nil, []string{"bin"}, nil)
	binDir := checkDir(ctx, t, ds, raDir, "bin", []string{"clang"}, nil, []string{"clang++", "clang++-1"})

	_, isExecutable, err := getDigest(binDir, "clang")
	if err != nil || !isExecutable {
		t.Errorf("clang is not executable: %t, %v; want: true, nil", isExecutable, err)
	}
	for _, symlink := range []string{"clang++", "clang++-1"} {
		_, _, err := getDigest(binDir, symlink)
		if err != nil {
			t.Errorf("%s not found", symlink)
		}
	}
	skiaDir := checkDir(ctx, t, ds, tpDir, "skia", nil, []string{"include"}, nil)
	skiaIncludeDir := checkDir(ctx, t, ds, skiaDir, "include", nil, []string{"private"}, nil)
	skiaPrivateDir := checkDir(ctx, t, ds, skiaIncludeDir, "private", []string{"SkSafe32.h"}, nil, nil)
	_, isExecutable, err = getDigest(skiaPrivateDir, "SkSafe32.h")
	if err != nil || isExecutable {
		t.Errorf("SkSafe32 is executable: %t %v; want: false, nil", isExecutable, err)
	}
}

func TestBuildWithSubTree(t *testing.T) {
	ctx := t.Context()
	ds := digest.NewStore()
	mt := New(ds)

	for _, ent := range []Entry{
		{
			Name:         "bin/clang",
			Data:         digest.FromBytes("clang binary", []byte("clang binary")),
			IsExecutable: true,
		},
		{
			Name:   "bin/clang++",
			Target: "clang",
		},
		{
			Name: "lib/clang/17/include/stdint.h",
			Data: digest.FromBytes("stdint.h", []byte("stdint.h")),
		},
		{
			Name: "lib/libstdc++.so.6",
			Data: digest.FromBytes("libstdc++.so.6", []byte("libstdc++.so.6")),
		},
	} {
		err := mt.Set(ent)
		if err != nil {
			t.Fatalf("mt.Set(%v)=%v; want nil error", ent, err)
		}
	}
	d, err := mt.Build(ctx)
	if err != nil {
		t.Fatalf("mt.Build()=%v, %v; want nil err", d, err)
	}

	tds := ds
	ds = digest.NewStore()
	t.Logf("set tree third_party/llvm-build/Release+Asserts %s", d)
	mt = New(ds)
	err = mt.SetTree(TreeEntry{
		Name:   "third_party/llvm-build/Release+Asserts",
		Digest: d,
		Store:  tds,
	})
	if err != nil {
		t.Errorf("SetTree(treeEntry)=%v; want nil err", err)
	}
	for _, e := range []struct {
		ent     Entry
		wantErr bool
	}{
		{
			ent: Entry{
				Name:         "third_party/llvm-build/Release+Asserts/bin/clang",
				Data:         digest.FromBytes("clang binary", []byte("clang binary")),
				IsExecutable: true,
			},
			wantErr: true,
		},
		{
			ent: Entry{
				Name: "base/base.h",
				Data: digest.FromBytes("base.h", []byte("base.h")),
			},
		},
		{
			ent: Entry{
				Name: "base/base.cc",
				Data: digest.FromBytes("base.cc", []byte("base.cc")),
			},
		},
	} {
		err := mt.Set(e.ent)
		if (err != nil) != e.wantErr {
			t.Errorf("mt.Set(%v)=%v; want err %t", e.ent, err, e.wantErr)
		}
	}
	d, err = mt.Build(ctx)
	if err != nil {
		t.Fatalf("mt.Build()=%v, %v; want nil err", d, err)
	}

	dir, err := openDir(ctx, ds, d)
	if err != nil {
		t.Fatalf("root %v not found: %v", d, err)
	}
	checkDir(ctx, t, ds, dir, "",
		nil,
		[]string{"base", "third_party"},
		nil)
	checkDir(ctx, t, ds, dir, "base",
		[]string{"base.cc", "base.h"},
		nil, nil)

	tpDir := checkDir(ctx, t, ds, dir, "third_party", nil, []string{"llvm-build"}, nil)
	llvmDir := checkDir(ctx, t, ds, tpDir, "llvm-build", nil, []string{"Release+Asserts"}, nil)
	raDir := checkDir(ctx, t, ds, llvmDir, "Release+Asserts", nil, []string{"bin", "lib"}, nil)
	binDir := checkDir(ctx, t, ds, raDir, "bin", []string{"clang"}, nil, []string{"clang++"})

	_, isExecutable, err := getDigest(binDir, "clang")
	if err != nil || !isExecutable {
		t.Errorf("clang is not executable: %t %v; want: true, nil", isExecutable, err)
	}
	_, _, err = getDigest(binDir, "clang++")
	if err != nil {
		t.Errorf("clang++ not found: %v", err)
	}

	libDir := checkDir(ctx, t, ds, raDir, "lib", []string{"libstdc++.so.6"}, []string{"clang"}, nil)
	libClangDir := checkDir(ctx, t, ds, libDir, "clang", nil, []string{"17"}, nil)
	libClang17Dir := checkDir(ctx, t, ds, libClangDir, "17", nil, []string{"include"}, nil)
	libClang17IncludeDir := checkDir(ctx, t, ds, libClang17Dir, "include", []string{"stdint.h"}, nil, nil)
	_, isExecutable, err = getDigest(libClang17IncludeDir, "stdint.h")
	if err != nil || isExecutable {
		t.Errorf("stdint.h is executable: %t, %v; want: false, nil", isExecutable, err)
	}
}

func TestBuildDuplicateError(t *testing.T) {
	for _, tc := range []struct {
		desc string
		ents []Entry
	}{
		{
			desc: "dup file-file",
			ents: []Entry{
				{
					Name: "dir/file1",
					Data: digest.FromBytes("file1.1", []byte("file1.1")),
				},
				{
					Name: "dir/file1",
					Data: digest.FromBytes("file1.2", []byte("file1.2")),
				},
			},
		},
		{
			desc: "dup file-symlink",
			ents: []Entry{
				{
					Name: "dir/foo",
					Data: digest.FromBytes("foo file", []byte("foo file")),
				},
				{
					Name:   "dir/foo",
					Target: "bar",
				},
			},
		},
		{
			desc: "dup file-dir",
			ents: []Entry{
				{
					Name: "dir/foo",
					Data: digest.FromBytes("foo file", []byte("foo file")),
				},
				{
					Name: "dir/foo",
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := t.Context()
			ds := digest.NewStore()
			mt := New(ds)
			for _, ent := range tc.ents {
				err := mt.Set(ent)
				if err != nil {
					t.Fatalf("mt.Set(%q)=%v; want=nil", ent.Name, err)
				}
			}
			d, err := mt.Build(ctx)
			if err == nil {
				t.Errorf("mt.Build()=%v, nil, want=error", d)
			}
		})
	}
}

func TestBuildDuplicateSymlinkDir(t *testing.T) {
	ctx := t.Context()
	ds := digest.NewStore()
	mt := New(ds)
	ents := []Entry{
		{
			Name:   "dir/foo",
			Target: "bar",
		},
		{
			Name: "dir/foo",
		},
	}
	for _, ent := range ents {
		err := mt.Set(ent)
		if err != nil {
			t.Fatalf("mt.Set(%q)=%v; want=nil", ent.Name, err)
		}
	}
	d, err := mt.Build(ctx)
	if err != nil {
		t.Errorf("mt.Build()=%v, %v, want nil err", d, err)
	}
	dir, err := openDir(ctx, ds, d)
	if err != nil {
		t.Fatalf("root %v not found: %v", d, err)
	}
	subdir := checkDir(ctx, t, ds, dir, "",
		nil,
		[]string{"dir"},
		nil)
	// use symlink rather than dir.
	// dir/bar is real directory, since dir/foo is symlink to bar
	// and dir/foo set as directory.
	checkDir(ctx, t, ds, subdir, "dir",
		nil,
		[]string{"bar"},
		[]string{"foo"})
}

func TestBuildResolveSymlinkDir(t *testing.T) {
	ctx := t.Context()
	ds := digest.NewStore()
	mt := New(ds)
	ents := []Entry{
		{
			Name:   "Foo.framework/Headers",
			Target: "Versions/Current/Headers",
		},
		{
			Name: "Foo.framework/Headers/Bar.h",
			Data: digest.FromBytes("Bar.h", []byte("Bar.h")),
		},
		{
			Name: "Foo.framework/Versions/Current/Headers",
		},
	}
	for _, ent := range ents {
		err := mt.Set(ent)
		if err != nil {
			t.Fatalf("mt.Set(%q)=%v; want=nil", ent.Name, err)
		}
	}
	d, err := mt.Build(ctx)
	if err != nil {
		t.Errorf("mt.Build()=%v, %v, want nil err", d, err)
	}
	dir, err := openDir(ctx, ds, d)
	if err != nil {
		t.Fatalf("root %v not found: %v", d, err)
	}
	ffDir := checkDir(ctx, t, ds, dir, "Foo.framework", nil, []string{"Versions"}, []string{"Headers"})
	vdir := checkDir(ctx, t, ds, ffDir, "Versions", nil, []string{"Current"}, nil)
	cdir := checkDir(ctx, t, ds, vdir, "Current", nil, []string{"Headers"}, nil)
	checkDir(ctx, t, ds, cdir, "Headers", []string{"Bar.h"}, nil, nil)
}

func TestBuildResolveSymlinkDirAndSymlinkFile(t *testing.T) {
	ctx := t.Context()
	ds := digest.NewStore()
	mt := New(ds)
	ents := []Entry{
		{
			Name: "system/core/include",
		},
		{
			Name:   "system/core/include/utils",
			Target: "../libutils/include/utils/",
		},
		{
			Name:   "system/core/include/utils/Errors.h",
			Target: "../../binder/include/utils/Errors.h",
		},
		{
			Name: "system/core/include/utils/RWLock.h",
			Data: digest.FromBytes("RWLock.h", []byte("#include <utils/Errors.h>\n")),
		},
		{
			Name: "system/core/libutils/binder/include/utils/Errors.h",
			Data: digest.FromBytes("Errors.h", []byte("")),
		},
		{
			Name: "system/core/libutils/include/utils",
		},
	}
	for _, ent := range ents {
		err := mt.Set(ent)
		if err != nil {
			t.Fatalf("mt.Set(%q)=%v; want=nil", ent.Name, err)
		}
	}
	d, err := mt.Build(ctx)
	if err != nil {
		t.Errorf("mt.Build()=%v, %v, want nil err", d, err)
	}
	dir, err := openDir(ctx, ds, d)
	if err != nil {
		t.Fatalf("root %v not found: %v", d, err)
	}
	// The expected tree looks like this:
	// .
	// └── system
	//     └── core
	//         ├── include
	//         │   └── utils -> ../libutils/include/utils/
	//         └── libutils
	//             ├── binder
	//             │   └── include
	//             │       └── utils
	//             │           └── Errors.h
	//             └── include
	//                 └── utils
	//                     ├── Errors.h -> ../../binder/include/utils/Errors.h
	//                     └── RWLock.h
	sDir := checkDir(ctx, t, ds, dir, "system", nil, []string{"core"}, nil)
	cDir := checkDir(ctx, t, ds, sDir, "core", nil, []string{"include", "libutils"}, nil)
	checkDir(ctx, t, ds, cDir, "include", nil, nil, []string{"utils"})
	libDir := checkDir(ctx, t, ds, cDir, "libutils", nil, []string{"binder", "include"}, nil)
	bDir := checkDir(ctx, t, ds, libDir, "binder", nil, []string{"include"}, nil)
	biDir := checkDir(ctx, t, ds, bDir, "include", nil, []string{"utils"}, nil)
	checkDir(ctx, t, ds, biDir, "utils", []string{"Errors.h"}, nil, nil)
	liDir := checkDir(ctx, t, ds, libDir, "include", nil, []string{"utils"}, nil)
	checkDir(ctx, t, ds, liDir, "utils", []string{"RWLock.h"}, nil, []string{"Errors.h"})
}

func TestBuildResolveSymlinkTree(t *testing.T) {
	ctx := t.Context()
	ds := digest.NewStore()
	mt := New(ds)
	err := mt.SetTree(TreeEntry{
		Name:   "build/mac_files/SDKs/MacOSX.sdk",
		Digest: digest.Empty,
		Store:  ds,
	})
	if err != nil {
		t.Fatalf("mt.SetTree=%v; want nil", err)
	}
	ents := []Entry{
		{
			Name: "build/mac_files/SDKs/MacOSX.sdk",
		},
		{
			Name:   "build/mac_files/SDKs/MacOSX14.0.sdk",
			Target: "MacOSX.sdk",
		},
	}
	for _, ent := range ents {
		err := mt.Set(ent)
		if err != nil {
			t.Fatalf("mt.Set(%q)=%v; want=nil", ent.Name, err)
		}
	}
	d, err := mt.Build(ctx)
	if err != nil {
		t.Errorf("mt.Build()=%v, %v; want nil err", d, err)
	}
	dir, err := openDir(ctx, ds, d)
	if err != nil {
		t.Fatalf("root %v not found: %v", d, err)
	}
	bDir := checkDir(ctx, t, ds, dir, "build", nil, []string{"mac_files"}, nil)
	mDir := checkDir(ctx, t, ds, bDir, "mac_files", nil, []string{"SDKs"}, nil)
	checkDir(ctx, t, ds, mDir, "SDKs", nil, []string{"MacOSX.sdk"}, []string{"MacOSX14.0.sdk"})
}

func TestBuildResolveSymlinkDirMerge(t *testing.T) {
	ctx := t.Context()
	ds := digest.NewStore()
	mt := New(ds)
	ents := []Entry{
		{
			Name: "external/puffin",
		},
		{
			Name:   "external/puffin/puffin/src",
			Target: "../src",
		},
		{
			Name: "external/puffin/puffin/src/include/puffin",
		},
		{
			Name: "external/puffin/puffin/src/include/puffin/common.h",
			Data: digest.FromBytes("common.h", []byte("// common.h")),
		},
		{
			Name: "external/puffin/puffin/src/logging.h",
			Data: digest.FromBytes("logging.h", []byte("// logging.h")),
		},
		{
			Name: "external/puffin/src",
		},
		{
			Name: "external/puffin/src/include",
		},
		{
			Name: "external/puffin/src/puff_reader.cc",
			Data: digest.FromBytes("puff_reader.cc", []byte("// puff_reader.cc")),
		},
	}
	for _, ent := range ents {
		err := mt.Set(ent)
		if err != nil {
			t.Fatalf("mt.Set(%q)=%v; want=nil", ent.Name, err)
		}
	}
	d, err := mt.Build(ctx)
	if err != nil {
		t.Errorf("mt.Build()=%v, %v; want nil err", d, err)
	}
	dir, err := openDir(ctx, ds, d)
	if err != nil {
		t.Fatalf("root %v not found: %v", d, err)
	}
	eDir := checkDir(ctx, t, ds, dir, "external", nil, []string{"puffin"}, nil)
	pDir := checkDir(ctx, t, ds, eDir, "puffin", nil, []string{"puffin", "src"}, nil)
	checkDir(ctx, t, ds, pDir, "puffin", nil, nil, []string{"src"})
	sDir := checkDir(ctx, t, ds, pDir, "src", []string{"logging.h", "puff_reader.cc"}, []string{"include"}, nil)

	// properly merge external/puffin/puffin/src/include/** into
	// external/puffin/src/include
	iDir := checkDir(ctx, t, ds, sDir, "include", nil, []string{"puffin"}, nil)
	checkDir(ctx, t, ds, iDir, "puffin", []string{"common.h"}, nil, nil)
}

func checkDir(ctx context.Context, t *testing.T, ds *digest.Store, pdir *rpb.Directory, name string, wantFiles []string, wantDirs []string, wantSymlinks []string) *rpb.Directory {
	t.Helper()
	t.Logf("check %s", name)
	dir := pdir
	if name != "" {
		d, _, err := getDigest(pdir, name)
		if err != nil {
			t.Fatalf("getDigest(pdir, %q)=_, _, %v; want nil err", name, err)
		}
		dir, err = openDir(ctx, ds, d)
		if err != nil {
			t.Fatalf("openDir(ds, %v)=_, %v; want nil err", d, err)
		}
	}
	t.Logf("dir: %s", dir)
	files, dirs, symlinks := readDir(dir)
	if !cmp.Equal(files, wantFiles) {
		t.Errorf("files=%q; want=%q", files, wantFiles)
	}
	if !cmp.Equal(dirs, wantDirs) {
		t.Errorf("dirs=%q; want=%q", dirs, wantDirs)
	}
	if !cmp.Equal(symlinks, wantSymlinks) {
		t.Errorf("symlinks=%q; want=%q", symlinks, wantSymlinks)
	}
	return dir
}

func readDir(dir *rpb.Directory) (files, dirs, symlinks []string) {
	for _, e := range dir.Files {
		files = append(files, e.Name)
	}
	for _, e := range dir.Directories {
		dirs = append(dirs, e.Name)
	}
	for _, e := range dir.Symlinks {
		symlinks = append(symlinks, e.Name)
	}
	return files, dirs, symlinks
}

// Given a directory `dir` and an entry `name`, returns:
// - Digest of entry within `dir` with name=`name`. For symlinks, returns empty digest.
// - Whether it is executable. For symlinks, returns false even if the symlink's target can be executable.
func getDigest(dir *rpb.Directory, name string) (digest.Digest, bool, error) {
	for _, e := range dir.Files {
		if e.Name == name {
			return digest.FromProto(e.Digest), e.IsExecutable, nil
		}
	}
	for _, e := range dir.Symlinks {
		if e.Name == name {
			return digest.Digest{}, false, nil
		}
	}
	for _, e := range dir.Directories {
		if e.Name == name {
			return digest.FromProto(e.Digest), false, nil
		}
	}
	return digest.Digest{}, false, errors.New("not found")
}

func openDir(ctx context.Context, ds *digest.Store, d digest.Digest) (*rpb.Directory, error) {
	data, ok := ds.Get(d)
	if !ok {
		return nil, fmt.Errorf("%v not found", d)
	}
	dir := &rpb.Directory{}
	r, err := data.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	err = proto.Unmarshal(b, dir)
	if err != nil {
		return nil, err
	}
	return dir, err
}

func BenchmarkSetDir(b *testing.B) {
	ds := digest.NewStore()
	m := New(ds)
	cur := dirstate{name: ".", dir: m.m[""]}

	for b.Loop() {
		m.setDir(cur, "subdir")
	}
}
