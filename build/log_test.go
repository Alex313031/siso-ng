// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestPanicLocation(t *testing.T) {
	buf := []byte(`goroutine 1015 [running]:
infra/build/siso/build.(*Builder).runStep.func1()
	infra/build/siso/build/run_step.go:37 +0x89
panic({0xd8aec0, 0x165b9f0})
	runtime/panic.go:890 +0x263
infra/build/siso/reapi.(*Client).Missing(0x0, {0x102a9c8, 0xc01e25bcb0}, {0xc01e276000, 0xda, 0x0?})
	infra/build/siso/reapi/cas.go:178 +0x1cd
infra/build/siso/reapi.(*Client).UploadAll(0xc01e232b40?, {0x102a9c8?, 0xc0175a03f0?}, 0xc021a0f540)
	infra/build/siso/reapi/cas.go:200 +0x2dd
infra/build/siso/build.(*subtree).init(0xc01e1e0660, {0x102a9c8, 0xc0175a03f0},
0xc019aa5200, {0xc0223314d0, 0x26}, {0xc003a3f000?, 0x478157?, 0x102a880?})
	 infra/build/siso/build/tree_input.go:114 +0x8da
infra/build/siso/build.(*Builder).treeInput(0xc019aa5200, {0x102a9c8, 0xc0175a03f0}, {0xc0223314d0, 0x26}, {0xee0f2a, 0x8})
	infra/build/siso/build/tree_input.go:54 +0x1f7
infra/build/siso/build.(*Builder).treeInputs(0xc02549f380?, {0x102a9c8, 0xc0175a03f0}, {0xee0f2a, 0x8}, {0xc0131feac0?, 0x2, 0x159c77c6fa2a79ee?}, {0xc02718f800, 0x4, ...})
	infra/build/siso/build/tree_input.go:25 +0x113
infra/build/siso/build.depsGCC.fixCmdInputs({}, {0x102a9c8, 0xc0175a03f0}, 0xc019aa5200, 0xc026353200)
	infra/build/siso/build/deps_gcc.go:63 +0x39b
infra/build/siso/build.depsGCC.DepsCmd({}, {0x102a9c8, 0xc0175a03f0}, 0xc0272b3650?, 0xc0153ffb00)
	infra/build/siso/build/deps_gcc.go:122 +0xbf
infra/build/siso/build.depsCmd({0x102a9c8, 0xc0175a03f0}, 0xee083e?, 0xc0153ffb00)
	infra/build/siso/build/deps.go:155 +0x19a
infra/build/siso/build.preprocCmd({0x102a9c8?, 0xc0175a0270?}, 0xc0175a0270?, 0xc0153ffb00)
	infra/build/siso/build/preproc.go:30 +0xad
infra/build/siso/build.(*Builder).runStep.func3({0x102a9c8, 0xc0175a0270})
	infra/build/siso/build/run_step.go:113 +0x65
infra/build/siso/sync/semaphore.(*Semaphore).Do(0x102a9c8?, {0x102a9c8?, 0xc019181d40?}, 0xc0272b3bb8)
	infra/build/siso/sync/semaphore/semaphore.go:114 +0x7a
infra/build/siso/build.(*Builder).runStep(0xc019aa5200, {0x102a9c8, 0xc019181d40}, 0xc0153ffb00)`)

	got := panicLocation(buf)
	want := []byte(`infra/build/siso/reapi.(*Client).Missing(0x0, {0x102a9c8, 0xc01e25bcb0}, {0xc01e276000, 0xda, 0x0?})
	infra/build/siso/reapi/cas.go:178 +0x1cd`)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("panicLocation(...)=%q; want %q; diff -want +got:\n%s", got, want, diff)
	}
}
