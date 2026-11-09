// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"path"
	"sync"
	"time"

	log "github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	grpcproto "google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/retry"
)

// walkDirEntry is a directory produced by a walkDirIter iterator.
// d is the digest of dir: the digest it was looked up by in walkdirCache or
// fetched by from CAS, or, for a directory streamed by GetTree, the digest
// of its serialization.
type walkDirEntry struct {
	d   digest.Digest
	dir *rpb.Directory
}

// loadWalkdir returns the directory d from walkdirCache or LocalCache.
func (c *Client) loadWalkdir(ctx context.Context, d digest.Digest) (*rpb.Directory, bool) {
	v, ok := c.walkdirCache.Load(d)
	if ok {
		return v.(*rpb.Directory), true
	}
	if c.opt.LocalCache == nil {
		return nil, false
	}
	dir := &rpb.Directory{}
	err := c.opt.LocalCache.Proto(ctx, d, dir)
	if err != nil {
		return nil, false
	}
	v, _ = c.walkdirCache.LoadOrStore(d, dir)
	return v.(*rpb.Directory), true
}

func (c *Client) storeWalkdir(ctx context.Context, d digest.Digest, dir *rpb.Directory) {
	c.walkdirCache.Store(d, dir)
	if c.opt.LocalCache != nil {
		_ = c.opt.LocalCache.SetProto(ctx, d, dir)
	}
}

// WalkDir walks the directory tree rooted identified by d,
// calling fn for each directory in the tree, including root.
// dir is shared with other walks and must not be modified.
func (c *Client) WalkDir(ctx context.Context, d digest.Digest, fn func(dname string, dd digest.Digest, dir *rpb.Directory) error) error {
	if c == nil {
		return fmt.Errorf("reapi is not configured")
	}
	ctx, span := trace.NewSpan(ctx, "reapi-walkdir")
	defer span.Close(nil)

	ws := walkStatePool.Get().(*walkState)
	defer ws.release()
	dirs := ws.dirs       // dname -> digest
	waiting := ws.waiting // digest -> dnames
	waiting[d] = append(waiting[d], "")
	known := ws.known
	var stats walkDirStats
	err := retry.Do(ctx, func() error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		for {
			var handleDir func(d digest.Digest, dir *rpb.Directory) error
			handleDir = func(d digest.Digest, dir *rpb.Directory) error {
				if len(waiting[d]) == 0 {
					return nil
				}
				var dnames []string
				dnames, waiting[d] = waiting[d], nil
				for _, dname := range dnames {
					dirs[dname] = d
					started := time.Now()
					err := fn(dname, d, dir)
					stats.cbDur += time.Since(started)
					stats.ndirs++
					if err != nil {
						return err
					}
					for _, subdir := range dir.Directories {
						sd := digest.FromProto(subdir.Digest)
						subname := path.Join(dname, subdir.Name)
						waiting[sd] = append(waiting[sd], subname)
						if sdir := known[sd]; sdir != nil {
							// check loop
							for pname := dname; pname != "" && pname != "."; pname = path.Dir(pname) {
								if dirs[pname] == sd {
									return fmt.Errorf("directory loop %s from %s -> %s", sd, subname, pname)
								}
							}
							err := handleDir(sd, sdir)
							if err != nil {
								return err
							}
						}
					}
				}
				return nil
			}
			for ent, err := range c.walkDirIter(ctx, d, &stats) {
				if err != nil {
					return err
				}
				dd, dir := ent.d, ent.dir
				if known[dd] != nil {
					// WalkDir already handled this directory.
					if log.V(1) {
						clog.Infof(ctx, "duplicate dir %s", dd)
					}
					continue
				}
				known[dd] = dir
				err := handleDir(dd, dir)
				if err != nil {
					return err
				}
			}
			break
		}
		return nil
	})
	if log.V(1) {
		clog.Infof(ctx, "walkdir %s %s", d, stats.String())
	}
	return err
}

// walkState holds the maps of one WalkDir call, reused via walkStatePool.
type walkState struct {
	dirs    map[string]digest.Digest
	waiting map[digest.Digest][]string
	known   map[digest.Digest]*rpb.Directory
}

// walkStatePool holds cleared walkStates for later WalkDir calls.
var walkStatePool = sync.Pool{
	New: func() any {
		return &walkState{
			dirs:    make(map[string]digest.Digest),
			waiting: make(map[digest.Digest][]string),
			known:   make(map[digest.Digest]*rpb.Directory),
		}
	},
}

// maxPooledWalkDirs caps the entries in each map of a pooled walkState.
// It is above the number of directory names in the input trees two phase
// caching usually walks, so most walks reuse the maps, and a pooled
// walkState holds at most about 1 MB.
const maxPooledWalkDirs = 1 << 12

// release clears ws and puts it back into walkStatePool, unless one of
// its maps grew above maxPooledWalkDirs entries.
func (ws *walkState) release() {
	if len(ws.dirs) > maxPooledWalkDirs || len(ws.waiting) > maxPooledWalkDirs || len(ws.known) > maxPooledWalkDirs {
		return
	}
	clear(ws.dirs)
	clear(ws.waiting)
	clear(ws.known)
	walkStatePool.Put(ws)
}

type walkDirStats struct {
	pageToken     string
	apiDur, cbDur time.Duration
	nrecvs, ndirs int
	ncached       int
	nhit          int
}

func (s *walkDirStats) String() string {
	var avgAPIDur time.Duration
	if s.nrecvs > 0 {
		avgAPIDur = s.apiDur / time.Duration(s.nrecvs)
	}
	var avgCBDur time.Duration
	if s.ndirs > 0 {
		avgCBDur = s.cbDur / time.Duration(s.ndirs)
	}
	return fmt.Sprintf("api %d %s avg:%s dir %d cb %s avg:%s cached %d hit %d",
		s.nrecvs, s.apiDur, avgAPIDur, s.ndirs, s.cbDur, avgCBDur, s.ncached, s.nhit)
}

func (c *Client) walkDirIter(ctx context.Context, d digest.Digest, stats *walkDirStats) iter.Seq2[walkDirEntry, error] {
	_, ok := c.loadWalkdir(ctx, d)
	if ok {
		iter, err := c.cachedTreeIter(ctx, d, stats)
		if err == nil {
			return iter
		}
	}
	if c.opt.WalkDirStream {
		return c.getTreeIter(ctx, d, stats)
	}
	return c.readDirIter(ctx, d, stats)
}

func (c *Client) cachedTreeIter(ctx context.Context, d digest.Digest, stats *walkDirStats) (iter.Seq2[walkDirEntry, error], error) {
	var dirs []walkDirEntry
	pendings := []digest.Digest{d}
	seen := make(map[digest.Digest]struct{})
	seen[d] = struct{}{}
	for len(pendings) > 0 {
		d := pendings[0]
		pendings = pendings[1:]
		dir, ok := c.loadWalkdir(ctx, d)
		if !ok {
			return nil, fmt.Errorf("no directory for %s", d)
		}
		stats.nhit++
		dirs = append(dirs, walkDirEntry{d: d, dir: dir})
		for _, subdir := range dir.Directories {
			sd := digest.FromProto(subdir.Digest)
			if _, ok := seen[sd]; ok {
				continue
			}
			pendings = append(pendings, sd)
			seen[sd] = struct{}{}
		}
	}
	return func(yield func(walkDirEntry, error) bool) {
		for _, ent := range dirs {
			if !yield(ent, nil) {
				return
			}
		}
	}, nil
}

func (c *Client) getTreeIter(ctx context.Context, d digest.Digest, stats *walkDirStats) iter.Seq2[walkDirEntry, error] {
	casClient := rpb.NewContentAddressableStorageClient(c.casConn)
	return func(yield func(walkDirEntry, error) bool) {
		seen := make(map[digest.Digest]struct{})
		waits := make(map[digest.Digest]struct{})
		waits[d] = struct{}{}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// grpc.ForceCodecV2 is experimental. When upgrading grpc, check
		// that it still exists and that an empty codec name still keeps
		// the content-type "application/grpc" (treeCAS in walkdir_test.go
		// checks it).
		stream, err := casClient.GetTree(ctx, &rpb.GetTreeRequest{
			InstanceName:   c.opt.Instance,
			RootDigest:     d.Proto(),
			PageToken:      stats.pageToken,
			DigestFunction: c.digestFn.Value(),
		}, grpc.ForceCodecV2(rawTreeCodec{}))
		if err != nil {
			yield(walkDirEntry{}, err)
			return
		}
		// resp.dirs point into a grpc buffer that resp.free returns
		// to grpc's pool for reuse. Anything kept after free must be
		// copied out of it: proto.Unmarshal copies, and so does parse
		// for nextPageToken.
		var resp *rawTreeResponse
		defer func() { resp.free() }()
		for len(waits) > 0 {
			started := time.Now()
			resp, err = recvRawTree(stream)
			dur := time.Since(started)
			stats.apiDur += dur
			stats.nrecvs++
			if log.V(1) {
				var n int
				if resp != nil {
					n = len(resp.dirs)
				}
				clog.Infof(ctx, "recv %d %v", n, err)
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(walkDirEntry{}, err)
				return
			}
			// Directories streamed by GetTree carry no digest, and GetTree
			// sends a directory once for every place it occurs in the tree.
			// So hash the wire bytes first and decode only directories the
			// walk still waits for. Those taken from walkdirCache below keep
			// their cache key.
			type pending struct {
				raw []byte
				ent walkDirEntry
			}
			dirs := make([]pending, 0, len(resp.dirs))
			for _, raw := range resp.dirs {
				dirs = append(dirs, pending{raw: raw})
			}
			for len(dirs) > 0 {
				p := dirs[0]
				dirs = dirs[1:]
				dir, dd := p.ent.dir, p.ent.d
				if dir == nil {
					dd = c.digestFn.FromBytes(p.raw)
					if _, ok := seen[dd]; ok {
						// Already yielded, or queued from walkdirCache.
						delete(waits, dd)
						continue
					}
					// If the wire bytes hash to a digest a parent
					// listed, they are that directory.
					_, listed := waits[dd]
					if listed {
						// loadWalkdir missed dd when a parent listed
						// it. Another walk may have stored it since:
						// share that copy instead of decoding a
						// second one.
						if v, ok := c.walkdirCache.Load(dd); ok {
							dir = v.(*rpb.Directory)
						}
					}
					if dir == nil {
						dir = &rpb.Directory{}
						if err := proto.Unmarshal(p.raw, dir); err != nil {
							// Internal, as grpc reports a message it
							// cannot decode, so that WalkDir retries.
							yield(walkDirEntry{}, status.Errorf(codes.Internal, "bad directory in GetTreeResponse: %v", err))
							return
						}
						if !listed {
							// No parent listed the hash of the wire
							// bytes: the directory came before its
							// parent, or the server encodes it
							// differently. Digest our own
							// serialization, as parents' digests are.
							data, err := proto.Marshal(dir)
							if err != nil {
								yield(walkDirEntry{dir: dir}, err)
								return
							}
							dd = c.digestFn.FromBytes(data)
							if _, ok := seen[dd]; ok {
								delete(waits, dd)
								continue
							}
						}
						c.storeWalkdir(ctx, dd, dir)
					}
					stats.ncached++
					seen[dd] = struct{}{}
				}
				delete(waits, dd)
				if !yield(walkDirEntry{d: dd, dir: dir}, nil) {
					return
				}
				for _, sdn := range dir.Directories {
					sd := digest.FromProto(sdn.GetDigest())
					if _, ok := waits[sd]; ok {
						stats.nhit++
						continue
					}
					if _, ok := seen[sd]; ok {
						stats.nhit++
						continue
					}
					subdir, ok := c.loadWalkdir(ctx, sd)
					if ok {
						stats.nhit++
						seen[sd] = struct{}{}
						dirs = append(dirs, pending{ent: walkDirEntry{d: sd, dir: subdir}})
						continue
					}
					waits[sd] = struct{}{}
				}

			}
			stats.pageToken = resp.nextPageToken
			resp.free()
		}
	}
}

// rawTreeResponse is a GetTreeResponse whose directories are kept as
// their wire bytes. dirs alias buf and are valid until free.
type rawTreeResponse struct {
	buf           mem.Buffer
	dirs          [][]byte
	nextPageToken string
}

// free returns buf to grpc's pool.
// It is safe to call on nil and more than once.
func (r *rawTreeResponse) free() {
	if r == nil || r.buf == nil {
		return
	}
	r.buf.Free()
	r.buf = nil
	r.dirs = nil
}

// rawMessage receives a message's wire bytes through rawTreeCodec.
type rawMessage struct {
	buf mem.Buffer
}

// rawTreeCodec is the proto codec, except that it hands a received
// message to a *rawMessage as grpc's buffer instead of decoding it.
// Decoding a GetTreeResponse into a placeholder message would
// copy every directory into unknown fields.
type rawTreeCodec struct{}

// Marshal encodes v with the proto codec.
func (rawTreeCodec) Marshal(v any) (mem.BufferSlice, error) {
	return encoding.GetCodecV2(grpcproto.Name).Marshal(v)
}

// Unmarshal keeps a reference to data in v if v is a *rawMessage, and
// decodes data into v with the proto codec otherwise.
func (rawTreeCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*rawMessage)
	if !ok {
		return encoding.GetCodecV2(grpcproto.Name).Unmarshal(data, v)
	}
	// MaterializeToBuffer takes its own reference; grpc frees data
	// after Unmarshal returns.
	m.buf = data.MaterializeToBuffer(mem.DefaultBufferPool())
	return nil
}

// Name is empty so that the call keeps the default content-type
// "application/grpc": grpc.ForceCodecV2 derives the content-subtype
// from it, and RBE rejects "application/grpc+proto".
func (rawTreeCodec) Name() string {
	return ""
}

// recvRawTree receives the next GetTreeResponse without decoding its
// directories. The stream must use rawTreeCodec. The caller must free
// the response.
func recvRawTree(stream grpc.ClientStream) (*rawTreeResponse, error) {
	var m rawMessage
	if err := stream.RecvMsg(&m); err != nil {
		return nil, err
	}
	resp := &rawTreeResponse{buf: m.buf}
	if err := resp.parse(m.buf.ReadOnlyData()); err != nil {
		resp.free()
		// Internal, as grpc reports a message it cannot decode, so
		// that WalkDir retries.
		return nil, status.Error(codes.Internal, err.Error())
	}
	return resp, nil
}

// parse sets r.dirs to the wire bytes of each directory in b, an
// encoded GetTreeResponse, and r.nextPageToken to its next_page_token.
// r.dirs alias b.
func (r *rawTreeResponse) parse(b []byte) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("bad GetTreeResponse: %w", protowire.ParseError(n))
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType: // directories
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return fmt.Errorf("bad GetTreeResponse: %w", protowire.ParseError(n))
			}
			r.dirs = append(r.dirs, v)
			b = b[n:]
		case num == 2 && typ == protowire.BytesType: // next_page_token
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return fmt.Errorf("bad GetTreeResponse: %w", protowire.ParseError(n))
			}
			r.nextPageToken = string(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return fmt.Errorf("bad GetTreeResponse: %w", protowire.ParseError(n))
			}
			b = b[n:]
		}
	}
	return nil
}

func (c *Client) readDirIter(ctx context.Context, d digest.Digest, stats *walkDirStats) iter.Seq2[walkDirEntry, error] {
	casClient := rpb.NewContentAddressableStorageClient(c.casConn)
	return func(yield func(walkDirEntry, error) bool) {
		pendings := []digest.Digest{d}
		seen := make(map[digest.Digest]struct{})
		seen[d] = struct{}{}
		visit := func(ent walkDirEntry) bool {
			if !yield(ent, nil) {
				return false
			}
			for _, subdir := range ent.dir.Directories {
				sd := digest.FromProto(subdir.Digest)
				if _, ok := seen[sd]; ok {
					continue
				}
				pendings = append(pendings, sd)
				seen[sd] = struct{}{}
			}
			return true
		}
		for len(pendings) > 0 {
			req := &rpb.BatchReadBlobsRequest{
				InstanceName:   c.opt.Instance,
				DigestFunction: c.digestFn.Value(),
			}
			var cached []walkDirEntry
			for _, d := range pendings {
				dir, ok := c.loadWalkdir(ctx, d)
				if ok {
					stats.nhit++
					cached = append(cached, walkDirEntry{d: d, dir: dir})
					continue
				}
				req.Digests = append(req.Digests, d.Proto())
			}
			pendings = pendings[:0]
			var resp *rpb.BatchReadBlobsResponse
			if len(req.Digests) > 0 {
				started := time.Now()
				var err error
				resp, err = casClient.BatchReadBlobs(ctx, req)
				dur := time.Since(started)
				stats.apiDur += dur
				stats.nrecvs++
				if err != nil {
					yield(walkDirEntry{}, err)
					return
				}
			}
			for _, ent := range cached {
				if !visit(ent) {
					return
				}
			}
			for _, rd := range resp.GetResponses() {
				dd := digest.FromProto(rd.Digest)
				if rd.Status.GetCode() != 0 {
					yield(walkDirEntry{}, fmt.Errorf("blob %s resp: %w", dd, status.FromProto(rd.Status).Err()))
					return
				}
				data, err := c.decodeForBatchRead(dd, rd.Data, rd.Compressor)
				if err != nil {
					yield(walkDirEntry{}, fmt.Errorf("blob %s decode: %w", dd, err))
					return
				}
				// WalkDir and walkdirCache use dd as the digest of
				// data, so check it.
				if got := c.digestFn.FromBytes(data); got != dd {
					yield(walkDirEntry{}, fmt.Errorf("blob %s digest mismatch: got %s", dd, got))
					return
				}
				dir := &rpb.Directory{}
				err = proto.Unmarshal(data, dir)
				if err != nil {
					yield(walkDirEntry{}, fmt.Errorf("blob %s unmarshal: %w", dd, err))
					return
				}
				stats.ncached++
				c.storeWalkdir(ctx, dd, dir)
				if !visit(walkDirEntry{d: dd, dir: dir}) {
					return
				}
			}
		}
	}
}
