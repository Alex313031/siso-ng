// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package abfsutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

func newTestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "abfs.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &httptest.Server{
		Listener: lis,
		Config:   &http.Server{Handler: h},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return "unix://" + sockPath
}

func TestRegisterFiles_Unsupported(t *testing.T) {
	entries := []*Registration{
		{
			Entry: merkletree.Entry{
				Name: path.Path("out/Default/gen/foo.h"),
				Data: blob.NewData(nil, digest.Digest{Hash: "abc", SizeBytes: 10}),
			},
		},
	}

	tests := []struct {
		name   string
		client *Client
	}{
		{
			name:   "nil_client",
			client: nil,
		},
		{
			name:   "empty_client_struct",
			client: &Client{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			err := tc.client.RegisterFiles(ctx, "/src", entries)
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("RegisterFiles() = %v, want %v", err, errors.ErrUnsupported)
			}
		})
	}
}

func TestRegisterFiles_EmptyBody(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"
	buildDir := filepath.Join(rootDir, "out/Default")

	var gotReq setRBEDigestsReq
	var gotMethod, gotPath, gotContentType string

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Return 200 OK with empty body.
		w.WriteHeader(http.StatusOK)
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	d1 := digest.Digest{Hash: "hash1", SizeBytes: 123}
	d2 := digest.Digest{Hash: "hash2", SizeBytes: 456}

	entries := []*Registration{
		{
			Entry: merkletree.Entry{
				Name:         path.Path("gen/foo.h"),
				Data:         blob.NewData(nil, d1),
				IsExecutable: false,
			},
		},
		{
			Entry: merkletree.Entry{
				Name:         path.Path("bin/bar"),
				Data:         blob.NewData(nil, d2),
				IsExecutable: true,
			},
		},
	}

	err = client.RegisterFiles(ctx, buildDir, entries)
	if err != nil {
		t.Fatalf("RegisterFiles() = %v, want nil", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("request method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotPath != "/mnt/set-rbe-digests" {
		t.Errorf("request path = %q, want %q", gotPath, "/mnt/set-rbe-digests")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
	}

	wantReq := setRBEDigestsReq{
		Digests: []rbePathStat{
			{
				Path:   "out/Default/gen/foo.h",
				SHA256: "hash1",
				Size:   123,
				Mode:   uint32(syscall.S_IFREG | 0o644),
			},
			{
				Path:   "out/Default/bin/bar",
				SHA256: "hash2",
				Size:   456,
				Mode:   uint32(syscall.S_IFREG | 0o755),
			},
		},
	}

	if diff := cmp.Diff(wantReq, gotReq); diff != "" {
		t.Errorf("SetRBEDigestsReq diff (-want +got):\n%s", diff)
	}

	for i, ent := range entries {
		if ent.Err != nil {
			t.Errorf("entries[%d].Err = %v, want nil", i, ent.Err)
		}
	}
}

func TestRegisterFiles_PartialErrors(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"
	buildDir := filepath.Join(rootDir, "out/Default")

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]string{
			"out/Default/gen/failed.h": "permission denied",
		}
		respBody, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	entSuccess := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/success.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash-success", SizeBytes: 10}),
		},
	}
	entFailed := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/failed.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash-failed", SizeBytes: 20}),
		},
	}

	entries := []*Registration{entSuccess, entFailed}
	err = client.RegisterFiles(ctx, buildDir, entries)
	if err != nil {
		t.Fatalf("RegisterFiles() = %v, want nil", err)
	}

	if entSuccess.Err != nil {
		t.Errorf("entSuccess.Err = %v, want nil", entSuccess.Err)
	}
	if entFailed.Err == nil || entFailed.Err.Error() != "permission denied" {
		t.Errorf("entFailed.Err = %v, want %q", entFailed.Err, "permission denied")
	}
}

func TestRegisterFiles_EmptyJSONResponse(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"
	buildDir := filepath.Join(rootDir, "out/Default")

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	ent := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/foo.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash1", SizeBytes: 10}),
		},
	}
	err = client.RegisterFiles(ctx, buildDir, []*Registration{ent})
	if err != nil {
		t.Fatalf("RegisterFiles() = %v, want nil", err)
	}
	if ent.Err != nil {
		t.Errorf("ent.Err = %v, want nil", ent.Err)
	}
}

func TestRegisterFiles_UnknownPathInResponse(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"
	buildDir := filepath.Join(rootDir, "out/Default")

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]string{
			"out/Default/unrelated.h": "some error",
		}
		respBody, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	ent := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/foo.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash1", SizeBytes: 10}),
		},
	}
	err = client.RegisterFiles(ctx, buildDir, []*Registration{ent})
	if err != nil {
		t.Fatalf("RegisterFiles() = %v, want nil", err)
	}
	if ent.Err != nil {
		t.Errorf("ent.Err = %v, want nil", ent.Err)
	}
}

func TestRegisterFiles_InvalidEntries(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"
	buildDir := filepath.Join(rootDir, "out/Default")

	var gotReq setRBEDigestsReq

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	entOutOfDir := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("../../../outside.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash-outside", SizeBytes: 10}),
		},
	}
	entEmptyDigest := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/empty.h"),
			Data: blob.Data{},
		},
	}
	entValid := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/valid.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash-valid", SizeBytes: 30}),
		},
	}

	entries := []*Registration{
		nil,
		entOutOfDir,
		entEmptyDigest,
		entValid,
	}

	err = client.RegisterFiles(ctx, buildDir, entries)
	if err != nil {
		t.Fatalf("RegisterFiles() = %v, want nil", err)
	}

	if entOutOfDir.Err == nil {
		t.Errorf("entOutOfDir.Err = nil, want error")
	}
	if entEmptyDigest.Err == nil {
		t.Errorf("entEmptyDigest.Err = nil, want error")
	}
	if entValid.Err != nil {
		t.Errorf("entValid.Err = %v, want nil", entValid.Err)
	}

	wantReq := setRBEDigestsReq{
		Digests: []rbePathStat{
			{
				Path:   "out/Default/gen/valid.h",
				SHA256: "hash-valid",
				Size:   30,
				Mode:   uint32(syscall.S_IFREG | 0o644),
			},
		},
	}
	if diff := cmp.Diff(wantReq, gotReq); diff != "" {
		t.Errorf("SetRBEDigestsReq diff (-want +got):\n%s", diff)
	}
}

func TestRegisterFiles_HTTPError(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"
	buildDir := filepath.Join(rootDir, "out/Default")

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	ent := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/foo.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash1", SizeBytes: 10}),
		},
	}
	err = client.RegisterFiles(ctx, buildDir, []*Registration{ent})
	if err == nil {
		t.Fatalf("RegisterFiles() = nil, want error")
	}
}

func TestRegisterFiles_InvalidJSONResponse(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"
	buildDir := filepath.Join(rootDir, "out/Default")

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{invalid-json"))
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	ent := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/foo.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash1", SizeBytes: 10}),
		},
	}
	err = client.RegisterFiles(ctx, buildDir, []*Registration{ent})
	if err == nil {
		t.Fatalf("RegisterFiles() = nil, want error")
	}
}

func TestRegisterFiles_NetworkError(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"
	buildDir := filepath.Join(rootDir, "out/Default")

	sockPath := filepath.Join(t.TempDir(), "abfs.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &httptest.Server{
		Listener: lis,
		Config:   &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})},
	}
	srv.Start()
	// Close immediately so network requests will fail.
	srv.Close()

	client, err := New(ctx, "unix://"+sockPath, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	ent := &Registration{
		Entry: merkletree.Entry{
			Name: path.Path("gen/foo.h"),
			Data: blob.NewData(nil, digest.Digest{Hash: "hash1", SizeBytes: 10}),
		},
	}
	err = client.RegisterFiles(ctx, buildDir, []*Registration{ent})
	if err == nil {
		t.Fatalf("RegisterFiles() = nil, want network error")
	}
}

func TestDigest(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/mnt/get-rbe-digest" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		p := r.URL.Query().Get("path")
		switch p {
		case "out/Default/gen/foo.h":
			resp := rbePathStat{
				Path:   p,
				SHA256: "hash-foo",
				Size:   100,
			}
			respBody, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(respBody)
		case "out/Default/gen/notfound.h":
			http.Error(w, "file not found", http.StatusNotFound)
		case "out/Default/gen/invalid_json.h":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{invalid-json"))
		case "out/Default/gen/server_error.h":
			http.Error(w, "internal server error", http.StatusInternalServerError)
		default:
			http.Error(w, fmt.Sprintf("unknown path %q", p), http.StatusBadRequest)
		}
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	t.Run("success", func(t *testing.T) {
		ctx := t.Context()
		d, err := client.Digest(ctx, filepath.Join(rootDir, "out/Default/gen/foo.h"))
		if err != nil {
			t.Fatalf("Digest() = %v, want nil", err)
		}
		want := digest.Digest{Hash: "hash-foo", SizeBytes: 100}
		if d != want {
			t.Errorf("Digest() = %v, want %v", d, want)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		ctx := t.Context()
		_, err := client.Digest(ctx, filepath.Join(rootDir, "out/Default/gen/notfound.h"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Digest() error = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("server_error", func(t *testing.T) {
		ctx := t.Context()
		_, err := client.Digest(ctx, filepath.Join(rootDir, "out/Default/gen/server_error.h"))
		if err == nil {
			t.Fatalf("Digest() = nil, want error")
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		ctx := t.Context()
		_, err := client.Digest(ctx, filepath.Join(rootDir, "out/Default/gen/invalid_json.h"))
		if err == nil {
			t.Fatalf("Digest() = nil, want error")
		}
	})

	t.Run("out_of_dir", func(t *testing.T) {
		ctx := t.Context()
		_, err := client.Digest(ctx, "/other/path.h")
		if err == nil {
			t.Fatalf("Digest() = nil, want error")
		}
	})

	t.Run("unsupported_client", func(t *testing.T) {
		ctx := t.Context()
		var nilClient *Client
		_, err := nilClient.Digest(ctx, filepath.Join(rootDir, "out/Default/gen/foo.h"))
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("Digest() = %v, want %v", err, errors.ErrUnsupported)
		}
	})
}

func TestNew_UnixSocket(t *testing.T) {
	tmpdir := t.TempDir()
	sockPath := filepath.Join(tmpdir, "test.sock")

	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}
	defer lis.Close()

	t.Run("abs_path", func(t *testing.T) {
		ctx := t.Context()
		client, err := New(ctx, "unix://"+sockPath, tmpdir)
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		defer client.Close()
	})

	t.Run("rel_path", func(t *testing.T) {
		ctx := t.Context()
		client, err := New(ctx, "unix://test.sock", tmpdir)
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		defer client.Close()
	})
}

func TestBatchDigests(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"

	var gotReq getRBEDigestsReq
	var gotMethod, gotPath, gotContentType string

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")

		if r.Method != http.MethodPost {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/mnt/get-rbe-digests" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Return spec-compliant digests in arbitrary (reverse) order to verify path mapping.
		resp := []rbePathStat{
			{
				Path:   "frameworks/base/symlink_to_target",
				SHA256: "4b227777d4dd1fc61c6f884f48641d02b4d121d3fd328cb08b5531fcacdabf8a",
				Size:   17,
				Mode:   41471,
			},
			{
				Path:   "out/soong/bin/protoc",
				SHA256: "7a35c10298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				Size:   25481200,
				Mode:   33261,
			},
			{
				Path:   "frameworks/base/core/java/android/app/Activity.java",
				SHA256: "b5a2c96250612366acfc8befe72989b4361624e3d415b4a01094ae6b82c409e9",
				Size:   182304,
				Mode:   33188,
			},
		}
		respBody, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	fnames := []string{
		filepath.Join(rootDir, "frameworks/base/core/java/android/app/Activity.java"),
		filepath.Join(rootDir, "out/soong/bin/protoc"),
		filepath.Join(rootDir, "frameworks/base/symlink_to_target"),
	}

	digests, err := client.BatchDigests(ctx, fnames)
	if err != nil {
		t.Fatalf("BatchDigests() = %v, want nil", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("request method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotPath != "/mnt/get-rbe-digests" {
		t.Errorf("request path = %q, want %q", gotPath, "/mnt/get-rbe-digests")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
	}

	wantReq := getRBEDigestsReq{
		Paths: []string{
			"frameworks/base/core/java/android/app/Activity.java",
			"out/soong/bin/protoc",
			"frameworks/base/symlink_to_target",
		},
	}
	if diff := cmp.Diff(wantReq, gotReq); diff != "" {
		t.Errorf("getRBEDigestsReq diff (-want +got):\n%s", diff)
	}

	wantDigests := []digest.Digest{
		{
			Hash:      "b5a2c96250612366acfc8befe72989b4361624e3d415b4a01094ae6b82c409e9",
			SizeBytes: 182304,
		},
		{
			Hash:      "7a35c10298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			SizeBytes: 25481200,
		},
		{
			Hash:      "4b227777d4dd1fc61c6f884f48641d02b4d121d3fd328cb08b5531fcacdabf8a",
			SizeBytes: 17,
		},
	}
	if diff := cmp.Diff(wantDigests, digests); diff != "" {
		t.Errorf("BatchDigests() diff (-want +got):\n%s", diff)
	}
}

func TestBatchDigests_Empty(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]"))
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	digests, err := client.BatchDigests(ctx, nil)
	if err != nil {
		t.Fatalf("BatchDigests(nil) = %v, want nil", err)
	}
	if len(digests) != 0 {
		t.Errorf("BatchDigests(nil) len = %d, want 0", len(digests))
	}
}

func TestBatchDigests_PartialResponse(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := []rbePathStat{
			{
				Path:   "frameworks/base/found.java",
				SHA256: "hash-found",
				Size:   123,
			},
		}
		respBody, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	fnames := []string{
		filepath.Join(rootDir, "frameworks/base/found.java"),
		filepath.Join(rootDir, "frameworks/base/missing.java"),
	}

	digests, err := client.BatchDigests(ctx, fnames)
	if err != nil {
		t.Fatalf("BatchDigests() = %v, want nil", err)
	}

	wantDigests := []digest.Digest{
		{
			Hash:      "hash-found",
			SizeBytes: 123,
		},
		{}, // missing file has zero digest
	}
	if diff := cmp.Diff(wantDigests, digests); diff != "" {
		t.Errorf("BatchDigests() diff (-want +got):\n%s", diff)
	}
}

func TestBatchDigests_OutOfDir(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"

	client, err := New(ctx, "http://dummy", rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	tests := []struct {
		name  string
		fname string
	}{
		{
			name:  "outside_root",
			fname: "/other/path.h",
		},
		{
			name:  "dotdot_traversal",
			fname: filepath.Join(rootDir, "../../outside.h"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.BatchDigests(ctx, []string{tc.fname})
			if err == nil {
				t.Fatalf("BatchDigests(%q) = nil, want error", tc.fname)
			}
		})
	}
}

func TestBatchDigests_Unsupported(t *testing.T) {
	tests := []struct {
		name   string
		client *Client
	}{
		{
			name:   "nil_client",
			client: nil,
		},
		{
			name:   "empty_client_struct",
			client: &Client{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			_, err := tc.client.BatchDigests(ctx, []string{"/src/foo.h"})
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("BatchDigests() = %v, want %v", err, errors.ErrUnsupported)
			}
		})
	}
}

func TestBatchDigests_HTTPError(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	_, err = client.BatchDigests(ctx, []string{filepath.Join(rootDir, "foo.h")})
	if err == nil {
		t.Fatalf("BatchDigests() = nil, want error")
	}
}

func TestBatchDigests_InvalidJSONResponse(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"

	endpoint := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{invalid-json"))
	}))

	client, err := New(ctx, endpoint, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	_, err = client.BatchDigests(ctx, []string{filepath.Join(rootDir, "foo.h")})
	if err == nil {
		t.Fatalf("BatchDigests() = nil, want error")
	}
}

func TestBatchDigests_NetworkError(t *testing.T) {
	ctx := t.Context()
	rootDir := "/src"

	sockPath := filepath.Join(t.TempDir(), "abfs.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &httptest.Server{
		Listener: lis,
		Config:   &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})},
	}
	srv.Start()
	// Close immediately so network requests will fail.
	srv.Close()

	client, err := New(ctx, "unix://"+sockPath, rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	_, err = client.BatchDigests(ctx, []string{filepath.Join(rootDir, "foo.h")})
	if err == nil {
		t.Fatalf("BatchDigests() = nil, want network error")
	}
}

func TestClient_InDir(t *testing.T) {
	ctx := t.Context()
	rootDir := filepath.Join(t.TempDir(), "src")
	client, err := New(ctx, "http://dummy", rootDir)
	if err != nil {
		t.Fatalf("abfsutil.New() = %v", err)
	}
	defer client.Close()

	var nilClient *Client
	if nilClient.InDir(filepath.Join(rootDir, "foo.h")) {
		t.Errorf("nilClient.InDir() = true, want false")
	}
	if !client.InDir(filepath.Join(rootDir, "out/Default/foo.h")) {
		t.Errorf("client.InDir(inside) = false, want true")
	}
	if client.InDir(filepath.Join(rootDir, "../outside.h")) {
		t.Errorf("client.InDir(outside) = true, want false")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRegisterFiles_Batching(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		rootDir := "/src"
		buildDir := filepath.Join(rootDir, "out/Default")

		var rpcCount atomic.Int32
		var totalDigests atomic.Int32

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rpcCount.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var req setRBEDigestsReq
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			totalDigests.Add(int32(len(req.Digests)))
			resp := make(map[string]string)
			for _, d := range req.Digests {
				if d.Path == "out/Default/gen/file_3.h" {
					resp[d.Path] = "failed on file 3"
				}
			}
			respBody, err := json.Marshal(resp)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(respBody)
		})

		client, err := New(ctx, "http://dummy", rootDir)
		if err != nil {
			t.Fatalf("abfsutil.New() = %v", err)
		}
		// use in-process handler to work with synctest bubble.
		client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			return rec.Result(), nil
		})
		defer client.Close()

		const numCallers = 10
		regs := make([]*Registration, numCallers)
		var wg sync.WaitGroup
		for i := range numCallers {
			regs[i] = &Registration{
				Entry: merkletree.Entry{
					Name: path.Path(fmt.Sprintf("gen/file_%d.h", i)),
					Data: blob.NewData(nil, digest.Digest{Hash: fmt.Sprintf("hash_%d", i), SizeBytes: int64(i + 1)}),
				},
			}
			ent := regs[i]
			wg.Go(func() {
				if err := client.RegisterFiles(ctx, buildDir, []*Registration{ent}); err != nil {
					t.Errorf("RegisterFiles() = %v, want nil", err)
				}
			})
		}
		wg.Wait()

		if got := rpcCount.Load(); got != 1 {
			t.Errorf("rpcCount = %d, want 1", got)
		}
		if got := totalDigests.Load(); got != numCallers {
			t.Errorf("totalDigests = %d, want %d", got, numCallers)
		}
		for i, reg := range regs {
			if i == 3 {
				if reg.Err == nil || reg.Err.Error() != "failed on file 3" {
					t.Errorf("regs[3].Err = %v, want %q", reg.Err, "failed on file 3")
				}
			} else if reg.Err != nil {
				t.Errorf("regs[%d].Err = %v, want nil", i, reg.Err)
			}
		}
	})
}
