// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/o11y/clog"
)

// testDigestStr123 is the digest string for []byte{1, 2, 3}.
const testDigestStr123 = "039058c6f2c0cb492c533b0a4d14ef77cc0f78abccced5287d84a1a2011cfb81/3"

func TestData(t *testing.T) {
	ctx := t.Context()

	name := "123"
	b := []byte{1, 2, 3}
	d := FromBytes(digest.SHA256, name, b)
	if d.IsZero() {
		t.Errorf("FromBytes(..., []byte{1, 2, 3}).IsZero() = true, want false")
	}
	bFromData, err := DataToBytes(ctx, d)
	if err != nil {
		t.Fatalf("FromBytes(..., []byte{1, 2, 3}).Bytes(ctx) = _, %v, want nil error", err)
	}
	if !bytes.Equal(bFromData, b) {
		t.Errorf("FromBytes(..., []byte{1, 2, 3}).Bytes(ctx) = %v, _, want %v", bFromData, b)
	}
	gotStr := d.String()
	wantStr := fmt.Sprintf("%s 123", testDigestStr123)
	if gotStr != wantStr {
		t.Errorf("FromBytes(%q, []byte{1, 2, 3}).Bytes(ctx) = %v, _, want %v", name, gotStr, wantStr)
	}

	zd := NewData(nil, digest.Digest{})
	if !zd.IsZero() {
		t.Errorf("NewData(nil, digest.Digest{}).IsZero() = false, want true")
	}

	// TODO(b/267409605): Add test for FromProtoMessage.
}

func TestByteSource(t *testing.T) {
	ctx := t.Context()

	name := "123"
	b := []byte{1, 2, 3}
	bs := byteSource{name, b}
	gotStr := bs.String()
	wantStr := "123"
	if gotStr != wantStr {
		t.Errorf("byteSource{%q, ...}.String() = %v, want %v", name, gotStr, wantStr)
	}
	rc, err := bs.Open(ctx)
	if err != nil {
		t.Fatalf("byteSource{...}.Open() = _, %v, want nil error", err)
	}
	defer rc.Close()

	gotBytes, _ := io.ReadAll(rc)
	if !bytes.Equal(gotBytes, b) {
		t.Errorf("rc.Read(gotBytes) sets %v to gotBytes, want %v", gotBytes, b)
	}
}

type localFileSource struct {
	name string
}

func (localFileSource) IsLocal() {}
func (l localFileSource) Open(context.Context) (io.ReadCloser, error) {
	f, err := os.Open(l.name)
	return f, err
}
func (l localFileSource) String() string {
	return fmt.Sprintf("file://%s", l.name)
}

func TestLocalFileSource(t *testing.T) {
	ctx := t.Context()

	dir := t.TempDir()
	fname := filepath.Join(dir, "123")
	b := []byte{1, 2, 3}
	err := os.WriteFile(fname, b, 0644)
	if err != nil {
		t.Fatalf("failed to write file %q. %v", fname, err)
	}

	d, err := FromLocalFile(ctx, digest.SHA256, localFileSource{fname})
	if err != nil {
		t.Fatalf("FromLocalFile(...) = _, %v, want nil error", err)
	}
	gotBytes, err := DataToBytes(ctx, d)
	if err != nil {
		t.Fatalf("d.Bytes(...) = _, %v, want nil error", err)
	}
	if !bytes.Equal(gotBytes, b) {
		t.Errorf("d.Bytes(...) = %v, _, want %v", gotBytes, b)
	}

	gotStr := d.String()
	wantStr := fmt.Sprintf("%s file://%s", testDigestStr123, fname)
	if gotStr != wantStr {
		t.Errorf("d.String() = %q, want %q", gotStr, wantStr)
	}
}

type localFileDigesterSource struct {
	localFileSource
	fsDigest digest.Digest
	fsErr    error
}

func (s localFileDigesterSource) FileDigestFromFS(context.Context) (digest.Digest, error) {
	return s.fsDigest, s.fsErr
}

func TestFromLocalFile_FileDigester(t *testing.T) {
	dir := t.TempDir()
	fname := filepath.Join(dir, "123")
	b := []byte{1, 2, 3}
	if err := os.WriteFile(fname, b, 0644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", fname, err)
	}

	fsDigest := digest.SHA256.FromBytes([]byte{4, 5, 6})
	fileDigest := digest.SHA256.FromBytes(b)
	tests := []struct {
		name       string
		fsDigest   digest.Digest
		fsErr      error
		wantDigest digest.Digest
		wantLog    string
	}{
		{
			name:       "digest_from_fs",
			fsDigest:   fsDigest,
			wantDigest: fsDigest,
			wantLog:    "",
		},
		{
			name:       "unsupported_falls_back_silently",
			fsErr:      errors.ErrUnsupported,
			wantDigest: fileDigest,
			wantLog:    "",
		},
		{
			name:       "wrapped_unsupported_falls_back_silently",
			fsErr:      fmt.Errorf("no xattr: %w", errors.ErrUnsupported),
			wantDigest: fileDigest,
			wantLog:    "",
		},
		{
			name:       "unexpected_error_logs_warning_and_falls_back",
			fsErr:      errors.New("io failure"),
			wantDigest: fileDigest,
			wantLog:    fmt.Sprintf("failed to get digest from FS for %q: io failure\n", fmt.Sprintf("file://%s", fname)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			ctx := clog.NewContext(t.Context(), clog.FromContext(t.Context()).WithWriter(&logBuf))
			src := localFileDigesterSource{
				name:     fname,
				fsDigest: tc.fsDigest,
				fsErr:    tc.fsErr,
			}

			got, err := FromLocalFile(ctx, digest.SHA256, src)
			if err != nil {
				t.Fatalf("FromLocalFile(ctx, SHA256, %q) failed: %v", src, err)
			}
			if got.Digest() != tc.wantDigest {
				t.Errorf("FromLocalFile(ctx, SHA256, %q).Digest() = %v, want %v", src, got.Digest(), tc.wantDigest)
			}
			if gotLog := logBuf.String(); gotLog != tc.wantLog {
				t.Errorf("FromLocalFile(ctx, SHA256, %q) log = %q, want %q", src, gotLog, tc.wantLog)
			}
		})
	}
}
