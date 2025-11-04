// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package bytestreamio provides io interfaces on bytestream service.
package bytestreamio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	pb "google.golang.org/genproto/googleapis/bytestream"

	"go.chromium.org/build/siso/o11y/clog"
)

// Exists checks for the existence of a resource by its resourceName.
func Exists(ctx context.Context, c pb.ByteStreamClient, resourceName string) error {
	rd, err := c.Read(ctx, &pb.ReadRequest{
		ResourceName: resourceName,
		ReadLimit:    1,
	})
	if err != nil {
		return err
	}
	_, err = rd.Recv()
	return err
}

// Open opens a reader on the bytestream for the resourceName.
// ctx will be used until the reader is closed.
func Open(ctx context.Context, c pb.ByteStreamClient, resourceName string) (*Reader, error) {
	rd, err := c.Read(ctx, &pb.ReadRequest{
		ResourceName: resourceName,
	})
	if err != nil {
		return nil, err
	}
	return &Reader{
		rd: rd,
	}, nil
}

// Reader is a reader on bytestream and implements io.Reader.
type Reader struct {
	rd  pb.ByteStream_ReadClient
	buf []byte
	// size of data already read from the bytestream.
	size int64
}

// Read reads data from bytestream.
// The maximum data chunk size would be determined by server side.
func (r *Reader) Read(buf []byte) (int, error) {
	if r.rd == nil {
		return 0, errors.New("failed to read: bad Reader")
	}
	if len(r.buf) > 0 {
		n := copy(buf, r.buf)
		r.buf = r.buf[n:]
		r.size += int64(n)
		return n, nil
	}
	resp, err := r.rd.Recv()
	if err != nil {
		return 0, err
	}
	// resp.Data may be empty.
	r.buf = resp.Data
	n := copy(buf, r.buf)
	r.buf = r.buf[n:]
	r.size += int64(n)
	return n, nil
}

// Size reports read size by Read.
func (r *Reader) Size() int64 {
	return r.size
}

// Create creates a writer on the bytestream for resourceName.
// ctx will be used until the rriter is closed.
func Create(ctx context.Context, c pb.ByteStreamClient, resourceName, name string) (*Writer, error) {
	sizeStr := path.Base(resourceName)
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad size in resource name %q: %v", resourceName, err)
	}
	wr, err := c.Write(ctx)
	if err != nil {
		return nil, err
	}
	return &Writer{
		name:    name,
		resname: resourceName,
		size:    size,
		wr:      wr,
	}, nil
}

// Writer is a writer on bytestream, and implemnets io.Writer.
type Writer struct {
	name    string // data source name
	resname string // resource name for upload destination
	size    int64
	wr      pb.ByteStream_WriteClient
	offset  int64

	// bytestream server will accept blobs by partial upload if
	// the same blobs are already uploaded by io.EOF of Send.
	// then, ok becomes true and we don't need to Send the rest of
	// data, so Write just returns success.  Close issues
	// CloseAndRecv and doesn't check offset.
	ok bool
}

// ErrBadCommittedSize is an error when committed size doesn't match.
type BadCommittedSizeError struct {
	CommittedSize int64
	Offset        int64
	Size          int64
	Name          string
}

func (e BadCommittedSizeError) Error() string {
	return fmt.Sprintf("unexpected committed_size=%d offset=%d size=%d for %s", e.CommittedSize, e.Offset, e.Size, e.Name)
}

// Write writes data to bytestream.
// The maximum data chunk size would be determined by server side,
// so don't pass larger chunk than maximum data chunk size.
func (w *Writer) Write(buf []byte) (int, error) {
	if w.wr == nil {
		return 0, errors.New("failed to write: bad Writer")
	}
	if w.ok {
		return len(buf), nil
	}
	n := len(buf)
	p := 0
	const streamBufSize = 2 * 1024 * 1024 // 2MB, since grpc default max recv size is 4MB.
	for p < n {
		bufsize := len(buf[p:])
		if bufsize > streamBufSize {
			bufsize = streamBufSize
		}
		err := w.wr.Send(&pb.WriteRequest{
			ResourceName: w.resname,
			WriteOffset:  w.offset,
			Data:         buf[p : p+bufsize],
		})
		if errors.Is(err, io.EOF) {
			// the blob already stored in CAS.
			w.ok = true
			clog.Infof(w.wr.Context(), "bytestream write %s for %s got EOF at %d: %v", w.resname, w.name, w.offset, err)
			return n, nil
		}
		if err != nil {
			return p, fmt.Errorf("failed to send for %s: %w", w.name, err)
		}
		w.offset += int64(bufsize)
		p += bufsize
	}
	return n, nil
}

// Close closes the writer.
func (w *Writer) Close() error {
	if w.wr == nil {
		return errors.New("bad Writer")
	}
	if !w.ok {
		// The service will not view the resource as 'complete'
		// until the client has sent a 'WriteRequest' with 'FinishWrite'
		// set to 'true'.
		err := w.wr.Send(&pb.WriteRequest{
			ResourceName: w.resname,
			WriteOffset:  w.offset,
			FinishWrite:  true,
			// The client may leave 'data' empty.
		})
		if err != nil {
			return fmt.Errorf("failed to finish for %s: %w", w.name, err)
		}
	}
	res, err := w.wr.CloseAndRecv()
	if err != nil {
		// TODO(yannic): Handle `ALREADY_EXISTS`.
		return fmt.Errorf("failed to close for %s: %w", w.name, err)
	}

	// Verify the committed size from the server is what we expect.
	//
	// For uncompressed blobs, `committed_size` is expected to always be `size`,
	// even if we end up not uploading all chunks because there was a concurrent
	// upload (possibly from another machine) that "won" uploading and the
	// server deduplicated the request. `res.CommittedSize == w.size` always
	// holds.
	//
	// For compressed uploads, the situation is more complex:
	//
	//   - `res.CommittedSize == w.size`: we may upload the blob, and the server
	//     responds with the actual size of the blob (i.e., after the server
	//     decompressed the data).
	//
	//   - `res.CommittedSize == -1`: another concurrent upload won server
	//     indicated it does not want to receive all bytes from this upload.
	//     This is common for servers that normally sent `w.offset` for
	//     successful compressed uploads since the server has no way of knowing
	//     how many bytes it would have received since it cannot predict the
	//     number of bytes the client would have sent.
	//
	// See https://github.com/bazelbuild/remote-apis/blob/e94a7ece2a1e8da1dcf278a0baf2edfe7baafb94/build/bazel/remote/execution/v2/remote_execution.proto#L277-L284

	switch res.CommittedSize {
	case w.size:
		return nil

	case -1:
		// Some servers use -1 as special value for `res.CommittedSize` when
		// rejecting an upload in case the blob already exists.
		if w.ok && strings.Contains(w.resname, "compressed-blobs") {
			return nil
		}
		fallthrough

	default:
		return BadCommittedSizeError{
			CommittedSize: res.CommittedSize,
			Offset:        w.offset,
			Size:          w.size,
			Name:          w.name,
		}
	}
}
