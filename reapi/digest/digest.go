// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package digest handles content digests of remote executon API.
//
// You can find the Digest proto in REAPI here:
// https://github.com/bazelbuild/remote-apis/blob/c1c1ad2c97ed18943adb55f06657440daa60d833/build/bazel/remote/execution/v2/remote_execution.proto#L633
package digest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/retry"
)

var copyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

// Empty is a digest of empty content.
var Empty = ofBytes([]byte{})

// EmptyTree is a digest of an empty tree (Tree message with empty root directory).
var EmptyTree = ofBytes(func() []byte { b, _ := proto.Marshal(&rpb.Tree{Root: &rpb.Directory{}}); return b }())

// Digest is a digest.
type Digest struct {
	Hash      string `json:"hash,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// ofBytes creates a Digest from bytes.
func ofBytes(b []byte) Digest {
	h := sha256.Sum256(b)
	return Digest{
		Hash:      hex.EncodeToString(h[:]),
		SizeBytes: int64(len(b)),
	}
}

// fromReader creates a Digest from io.Reader.
func fromReader(r io.Reader) (Digest, error) {
	h := sha256.New()
	bufp := copyBufPool.Get().(*[]byte)
	n, err := io.CopyBuffer(h, r, *bufp)
	copyBufPool.Put(bufp)
	if err != nil {
		return Digest{}, err
	}
	return Digest{
		Hash:      hex.EncodeToString(h.Sum(nil)),
		SizeBytes: n,
	}, nil
}

// FromProto converts from digest proto.
func FromProto(d *rpb.Digest) Digest {
	if d == nil {
		return Digest{}
	}
	return Digest{
		Hash:      d.Hash,
		SizeBytes: d.SizeBytes,
	}
}

// IsZero returns true when digest is zero value (equivalent with nil digest proto).
func (d Digest) IsZero() bool {
	return d.Hash == ""
}

// Proto returns digest proto.
func (d Digest) Proto() *rpb.Digest {
	if d.IsZero() {
		return nil
	}
	return &rpb.Digest{
		Hash:      d.Hash,
		SizeBytes: d.SizeBytes,
	}
}

// String returns string representation of the digest (hash/sizes_bytes).
func (d Digest) String() string {
	if d.IsZero() {
		return ""
	}
	return fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes)
}

const slowThroughputPerSec = 1 * 1024 * 1024

// FetchTimeout returns reasonable timeout to fetch d.
func (d Digest) FetchTimeout() time.Duration {
	// 99p latency of BatchReadBlobs is 1.72s and ByteStream.Read is 0.522s as of 2025-08 in rbe-chromium-trusted
	return max(time.Duration(d.SizeBytes/slowThroughputPerSec)*time.Second, 10*time.Second)
}

// ContextWithTimeout returns context with timeout appropriate for d.
func ContextWithTimeout(ctx context.Context, d Digest) (context.Context, context.CancelFunc) {
	timeout := max(time.Duration(d.SizeBytes/slowThroughputPerSec)*time.Second, 10*time.Minute)
	if timeout > 10*time.Minute {
		clog.Infof(ctx, "digest timeout for %s: %s", d, timeout)
	}
	return context.WithTimeout(ctx, timeout)
}

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
	digest Digest
	source Source
}

// NewData creates a Data from source and digest.
func NewData(src Source, d Digest) Data {
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
func (d Data) Digest() Digest {
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

// DataToBytes returns byte values from a Data.
// Note that it reads all content. It should not be used for large blob.
func DataToBytes(ctx context.Context, d Data) ([]byte, error) {
	if bs, ok := d.source.(byteSource); ok {
		return slices.Clone(bs.b), nil
	}
	if d.Digest().SizeBytes <= 0 {
		return nil, nil
	}
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
	return buf, err
}

// FromProtoMessage creates Data from proto message.
func FromProtoMessage(m proto.Message) (Data, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return Data{}, err
	}
	return FromBytes(fmt.Sprintf("%T", m), b), nil
}

// FromBytes creates data from raw byte values.
func FromBytes(name string, b []byte) Data {
	return Data{
		digest: ofBytes(b),
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
func FromLocalFile(ctx context.Context, src Source) (Data, error) {
	fd, ok := src.(fileDigester)
	if ok {
		d, err := fd.FileDigestFromXattr(ctx)
		if err == nil {
			return Data{
				digest: d,
				source: src,
			}, nil
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
	d, err := fromReader(f)
	if err != nil {
		return Data{}, err
	}
	return Data{
		digest: d,
		source: src,
	}, nil
}

type fileDigester interface {
	FileDigestFromXattr(context.Context) (Digest, error)
}

// LocalFileSource is a source for local file.
type LocalFileSource interface {
	Source
	IsLocal()
}
