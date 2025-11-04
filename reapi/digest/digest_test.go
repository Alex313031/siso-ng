// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package digest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// testDigestStr123 is the digest string for []byte{1, 2, 3}.
const testDigestStr123 = "039058c6f2c0cb492c533b0a4d14ef77cc0f78abccced5287d84a1a2011cfb81/3"

func TestDigest(t *testing.T) {
	// Regular case
	b := []byte{1, 2, 3}
	d := ofBytes(b)

	if d.String() != testDigestStr123 {
		t.Errorf("ofBytes(%v).String() = %s, want %s", b, d.String(), testDigestStr123)
	}

	p := d.Proto()
	if p == nil {
		t.Errorf("ofBytes(%v).Proto() = nil, want a Digest proto", b)
	}

	dFromProto := FromProto(p)
	if dFromProto != d {
		t.Errorf("FromProto(%v) = %v, want %v", p, dFromProto, d)
	}

	// From nil proto
	nild := FromProto(nil)
	if nild.IsZero() != true {
		t.Errorf("FromProto(nil).IsZero() = false, want true")
	}

	// Empty digest
	empty := ofBytes([]byte{})
	if empty.SizeBytes != 0 {
		t.Errorf("ofBytes([]byte{}).SizeBytes = %v, want 0", empty.SizeBytes)
	}
	if empty.IsZero() {
		t.Errorf("ofBytes([]byte{}).IsZero() = true, want false")
	}
}

func TestData(t *testing.T) {
	ctx := t.Context()

	name := "123"
	b := []byte{1, 2, 3}
	d := FromBytes(name, b)
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

	zd := NewData(nil, Digest{})
	if !zd.IsZero() {
		t.Errorf("NewData(nil, Digest{}).IsZero() = false, want true")
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

	d, err := FromLocalFile(ctx, localFileSource{fname})
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
