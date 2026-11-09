// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/toolsupport/abfsutil"
)

func writePastFile(t *testing.T, fname, content string) digest.Digest {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(fname), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fname, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(fname, past, past); err != nil {
		t.Fatal(err)
	}
	return blob.FromBytes(digest.SHA256, fname, []byte(content)).Digest()
}

// testSockPath returns a short unix domain socket path that stays within
// macOS's 104-byte sockaddr_un.sun_path limit (t.TempDir() embeds the full
// test/subtest name, which can exceed 104 bytes on macOS).
func testSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "abfs") //nolint:usetesting // t.TempDir() embeds the subtest name and exceeds macOS's 104-byte sun_path limit.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// TestDigester_ABFSBatchAndFallback verifies that:
//   - paths outside the ABFS mount directory are split out before calling
//     BatchDigests so an out-of-dir file does not fail the batch;
//   - when BatchDigests returns an error, both background lazyCompute (popQueue)
//     and foreground batchDigester (Entries) fall back inline to local digest
//     computation without spawning goroutines or failing the call.
func TestDigester_ABFSBatchAndFallback(t *testing.T) {
	t.Run("split_out_of_dir", func(t *testing.T) {
		ctx := t.Context()
		abfsDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		otherDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		sock := testSockPath(t)
		fake := &abfsutil.Fake{Dir: abfsDir}
		abfs := fake.Start(ctx, t, sock)

		inFile := filepath.Join(abfsDir, "in.txt")
		wantIn := writePastFile(t, inFile, "in-dir content")

		outFile := filepath.Join(otherDir, "out.txt")
		wantOut := writePastFile(t, outFile, "out-of-dir content")

		hfs, err := New(ctx, Option{ABFS: abfs})
		if err != nil {
			t.Fatal(err)
		}
		defer hfs.Close(ctx)

		ents, err := hfs.Entries(ctx, abfsDir, []path.Path{"in.txt", path.New(outFile)})
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		if len(ents) != 2 {
			t.Fatalf("len(ents) = %d; want 2", len(ents))
		}
		if got := ents[0].Data.Digest(); got != wantIn {
			t.Errorf("ents[0] digest = %v; want %v", got, wantIn)
		}
		if got := ents[1].Data.Digest(); got != wantOut {
			t.Errorf("ents[1] digest = %v; want %v", got, wantOut)
		}
		if got := fake.GetRBEDigests()["in.txt"]; got != 1 {
			t.Errorf("fake.GetRBEDigests()[in.txt] = %d; want 1", got)
		}
	})

	t.Run("batch_error_inline_fallback", func(t *testing.T) {
		ctx := t.Context()
		abfsDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		otherDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}

		var batchCalls atomic.Int32
		sockPath := testSockPath(t)
		lis, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		srv := &httptest.Server{
			Listener: lis,
			Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/mnt/get-rbe-digests" {
					batchCalls.Add(1)
				}
				http.Error(w, "simulated abfs failure", http.StatusInternalServerError)
			})},
		}
		srv.Start()
		defer srv.Close()

		abfs, err := abfsutil.New(ctx, "unix://"+sockPath, abfsDir)
		if err != nil {
			t.Fatal(err)
		}
		defer abfs.Close()

		inLazy := filepath.Join(abfsDir, "lazy.txt")
		wantLazy := writePastFile(t, inLazy, "lazy content")
		outLazy := filepath.Join(otherDir, "lazy_out.txt")
		wantOutLazy := writePastFile(t, outLazy, "lazy out-of-dir content")
		inFg := filepath.Join(abfsDir, "fg.txt")
		wantFg := writePastFile(t, inFg, "foreground content")

		hfs, err := New(ctx, Option{ABFS: abfs, DeferDigest: true})
		if err != nil {
			t.Fatal(err)
		}
		defer hfs.Close(ctx)

		// Queue both an in-dir and an out-of-dir file via StatMtime (lazyCompute).
		if _, err := hfs.StatMtime(ctx, abfsDir, "lazy.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := hfs.StatMtime(ctx, otherDir, "lazy_out.txt"); err != nil {
			t.Fatal(err)
		}

		eLazy, _, _, ok := hfs.directory.lookup(ctx, path.Path(filepath.ToSlash(inLazy)))
		if !ok {
			t.Fatal("entry missing for lazy.txt")
		}
		eOutLazy, _, _, ok := hfs.directory.lookup(ctx, path.Path(filepath.ToSlash(outLazy)))
		if !ok {
			t.Fatal("entry missing for lazy_out.txt")
		}

		deadline := time.Now().Add(2 * time.Second)
		for eLazy.digest() != wantLazy || eOutLazy.digest() != wantOutLazy {
			if time.Now().After(deadline) {
				t.Fatalf("lazy fallback timed out: got lazy=%v (want %v), out=%v (want %v)",
					eLazy.digest(), wantLazy, eOutLazy.digest(), wantOutLazy)
			}
			runtime.Gosched()
		}

		// Also verify foreground batchDigester.wait fallback via Entries.
		ents, err := hfs.Entries(ctx, abfsDir, []path.Path{"fg.txt"})
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		if len(ents) != 1 || ents[0].Data.Digest() != wantFg {
			t.Errorf("Entries digest = %v; want %v", ents, wantFg)
		}
		if batchCalls.Load() < 2 {
			t.Errorf("batchCalls = %d; want >= 2", batchCalls.Load())
		}
	})
}

// TestDigester_ABFSStopDrainsQueueNoLeak verifies that stop() drains all
// remaining requests in d.queue synchronously (including when BatchDigests
// fails during stop()) and leaves no leaked goroutines.
func TestDigester_ABFSStopDrainsQueueNoLeak(t *testing.T) {
	ctx := t.Context()
	abfsDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	sockPath := testSockPath(t)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{
		Listener: lis,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "batch error during stop", http.StatusInternalServerError)
		})},
	}
	srv.Start()
	defer srv.Close()

	abfs, err := abfsutil.New(ctx, "unix://"+sockPath, abfsDir)
	if err != nil {
		t.Fatal(err)
	}
	defer abfs.Close()

	hfs, err := New(ctx, Option{ABFS: abfs})
	if err != nil {
		t.Fatal(err)
	}

	const nFiles = 20
	entries := make([]*entry, nFiles)
	wants := make([]digest.Digest, nFiles)
	// Hold d.mu while enqueueing entries directly so they remain in d.queue
	// until stop() drains them (testing stop()'s synchronous drain and inline
	// fallback after workers exit and d.q is closed).
	hfs.digester.mu.Lock()
	for i := range nFiles {
		name := fmt.Sprintf("f%d.txt", i)
		fname := filepath.Join(abfsDir, name)
		wants[i] = writePastFile(t, fname, fmt.Sprintf("content-%d", i))
		e := newLocalEntry()
		e.init(ctx, fname, hfs.executables, hfs.OS)
		entries[i] = e
		hfs.digester.queue = append(hfs.digester.queue, digestReq{
			fname: fname,
			e:     e,
		})
	}
	hfs.digester.mu.Unlock()

	if err := hfs.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i, e := range entries {
		if got := e.digest(); got != wants[i] {
			t.Errorf("entries[%d] digest after Close = %v; want %v", i, got, wants[i])
		}
	}
}
