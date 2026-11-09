// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package blob pairs content with its digest and provides an in-memory
// content-addressable store. It sits on top of hashigo/digest, which computes
// the digests; the sourcing, in-memory caching, and collection of blobs are
// siso concerns and live here.
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/retry"
)

// Source is the interface that opens a data source.
// It can be remote or local source.
// If this interface is implemented based on gRPC streaming for remote sources,
// the caller may need to retry Open/Read/Close in addition.
type Source interface {
	// Open returns io.ReadCloser of the source.
	Open(context.Context) (io.ReadCloser, error)

	// String returns the name of the data source.
	String() string
}

// Data is a data instance that consists of Digest and Source.
// TODO(b/268407930): it may be possible to be merged with Source.
type Data struct {
	digest digest.Digest
	source Source
}

// NewData creates a Data from source and digest.
func NewData(src Source, d digest.Digest) Data {
	return Data{
		digest: d,
		source: src,
	}
}

// IsZero returns true when the Data is zero value struct.
func (d Data) IsZero() bool {
	return d.digest.IsZero()
}

// Digest returns the Digest of the data.
func (d Data) Digest() digest.Digest {
	return d.digest
}

// Open opens the data source.
func (d Data) Open(ctx context.Context) (io.ReadCloser, error) {
	return d.source.Open(ctx)
}

// String returns the digest and the source in string format.
func (d Data) String() string {
	return fmt.Sprintf("%v %v", d.digest, d.source)
}

// allBytesReader is an optional optimization a Source may implement to return
// its full content directly, e.g. a remote source that handles its own retries.
// DataToBytes uses it when available.
type allBytesReader interface {
	ReadAll(context.Context) ([]byte, error)
}

// DataToBytes returns byte values from a Data.
// Note that it reads all content. It should not be used for large blob.
func DataToBytes(ctx context.Context, d Data) ([]byte, error) {
	if bs, ok := d.source.(byteSource); ok {
		return slices.Clone(bs.b), nil
	}
	if d.Digest().SizeBytes <= 0 {
		return nil, nil
	}
	if ar, ok := d.source.(allBytesReader); ok {
		return ar.ReadAll(ctx)
	}
	// Sources may open remote readers, so retry the whole read on retriable
	// errors. Local I/O errors are not retriable and fail immediately.
	buf := make([]byte, d.Digest().SizeBytes)
	err := retry.Do(ctx, func() error {
		f, err := d.Open(ctx)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.ReadFull(f, buf)
		return err
	})
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// FromProtoMessage creates Data from proto message.
func FromProtoMessage(fn digest.Function, m proto.Message) (Data, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return Data{}, err
	}
	return FromBytes(fn, fmt.Sprintf("%T", m), b), nil
}

// FromBytes creates data from raw byte values.
func FromBytes(fn digest.Function, name string, b []byte) Data {
	return Data{
		digest: fn.FromBytes(b),
		source: byteSource{name: name, b: b},
	}
}

// byteSource implements Source for in-memory source with raw byte values.
type byteSource struct {
	name string
	b    []byte
}

func (b byteSource) Open(ctx context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.b)), nil
}

func (b byteSource) String() string {
	return b.name
}

// FromLocalFile creates Data from local file source.
// it requires LocalFileSource because it doesn't handle
// retriable err from src.
func FromLocalFile(ctx context.Context, fn digest.Function, src Source) (Data, error) {
	fd, ok := src.(fileDigester)
	if ok {
		d, err := fd.FileDigestFromFS(ctx)
		if err == nil {
			return Data{
				digest: d,
				source: src,
			}, nil
		}
		if !errors.Is(err, errors.ErrUnsupported) {
			clog.Warningf(ctx, "failed to get digest from FS for %q: %v", src, err)
		}
	}
	_, ok = src.(LocalFileSource)
	if !ok {
		return Data{}, fmt.Errorf("src=%T is not LocalFileSource", src)
	}

	f, err := src.Open(ctx)
	if err != nil {
		return Data{}, err
	}
	defer f.Close()
	// Git-framing digest functions need the content size up front. Obtain it
	// from the source when available; -1 means "unknown" (fine for non-git
	// functions, which learn the size while streaming).
	size := int64(-1)
	if s, ok := src.(sizeSource); ok {
		if n, err := s.Size(); err == nil {
			size = n
		}
	}
	d, err := fn.FromReader(f, size)
	if err != nil {
		return Data{}, err
	}
	return Data{
		digest: d,
		source: src,
	}, nil
}

type fileDigester interface {
	FileDigestFromFS(context.Context) (digest.Digest, error)
}

// sizeSource is an optional interface for sources that can report their content
// size without reading the whole stream.
type sizeSource interface {
	Size() (int64, error)
}

// LocalFileSource is a source for local file.
type LocalFileSource interface {
	Source
	IsLocal()
}
