// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/hashfs/osfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/mmapfile"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/toolsupport/cartfsutil"
	"go.chromium.org/build/siso/toolsupport/cogutil"
)

const defaultStateFile = ".siso_fs_state"

// defaultCompressThreads is the default number of threads to use for data
// compression. Each thread compresses an independent chunk via zstd EncodeAll,
// so parallelism scales well up to the number of available cores. We limit
// the max parallelism to 32 due to benchmarks showing that more isn't useful
// considering the typical file size of hashfs state files, with decreasing
// gains and increased memory consumption.
var defaultCompressThreads = min(32, runtime.GOMAXPROCS(0))

// OutputLocalFunc returns true if given fname needs to be on local disk.
type OutputLocalFunc func(context.Context, string) bool

// IgnoreFunc returns true if given fname should be ignored in hashfs.
type IgnoreFunc func(context.Context, string) bool

// Option is an option for HashFS.
type Option struct {
	StateFile       string // filename that HashFS saves its state to
	CompressLevel   int    // compression level (0 = uncompressed, 1 = fastest, 10 = best)
	CompressThreads int    // number of threads to use for data compression
	UseMmap         bool   // use mmap for reading/writing state files

	KeepTainted bool // keep manually modified generated file

	DeferDigest bool // defer digest calculation to speed up nop build.

	MinFlushTimeout time.Duration // minimum timeout to flush operation (>= 10s)

	OSFSOption osfs.Option

	FSMonitor FSMonitor

	DataSource  DataSource
	OutputLocal OutputLocalFunc
	Ignore      IgnoreFunc
	CogFS       *cogutil.Client
	CartFS      *cartfsutil.Client

	SetStateLogger io.Writer // capture SetState log for test
}

// RegisterFlags registers flags for the option.
func (o *Option) RegisterFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&o.StateFile, "fs_state", defaultStateFile, "fs state filename")
	flagSet.IntVar(&o.CompressLevel, "fs_state_compression_level", 1, "fs state compression level (1 = fastest, 10 = best)")
	flagSet.IntVar(&o.CompressThreads, "fs_state_compression_threads", defaultCompressThreads, "number of threads to use for data compression")
	flagSet.BoolVar(&o.UseMmap, "fs_state_mmap", true, "use memory-mapped I/O for state file reads/writes")
	flagSet.BoolVar(&o.KeepTainted, "fs_keep_tainted", false, "keep manually modified generated file")
	flagSet.BoolVar(&o.DeferDigest, "fs_defer_digest", false, "defer digest calculation")
	flagSet.DurationVar(&o.MinFlushTimeout, "fs_min_flush_timeout", 30*time.Second, "minimum timeout for flush. ignored if it is shorter than 10s")
	o.OSFSOption.RegisterFlags(flagSet)
}

// DataSource is an interface to get digest source for digest and its name.
type DataSource interface {
	Source(context.Context, digest.Digest, string) digest.Source
}

const (
	// gzipMagic is the 2-byte, little endian magic number at the start of every gzip-compressed data.
	gzipMagic = 0x8B1F

	// zstdMagic is the 4-byte, little endian magic number at the start of every zstd frame.
	zstdMagic = 0xFD2FB528

	// zstdSkippableMagic is the 4-byte, little endian magic number used to identify our indexed, zstd-compressed file
	// format. It falls within the range of 0x184D2A50 to 0x184D2A5F, which are recognized by zstd as skippable frames,
	// ensuring that zstd decoders automatically ignore our index metadata. This makes our indexed file format backwards
	// and forwards compatible with Siso versions that attempt to decompress it as plain zstd-compressed data.
	zstdSkippableMagic = 0x184D2A50

	// zstdIndexVersion is the 4-byte, little endian magic number used to identify our current index format version.
	zstdIndexVersion = 0x51500001
)

// zstdEmptyFrame is a zstd frame that decompresses to zero bytes. It is
// written at the very start of the file so that older Siso versions (which
// only check the first four bytes for zstdMagic 0xFD2FB528) correctly
// identify the file as zstd-compressed. Standard decoders simply produce
// zero bytes for this frame and then continue with the skippable index
// frame and the data frames that follow.
//
//	28b52ffd 20 00 01 0000
//	│        │  │  └─── empty block (last block flag set, raw block type, size 0)
//	│        │  └────── frame content size (0 bytes)
//	│        └───────── frame header descriptor (no dict, no content checksum, single segment = true)
//	└────────┴───────── zstd magic number (0xFD2FB528, little endian)
var zstdEmptyFrame = []byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 0x00, 0x01, 0x00, 0x00}

func isGzip(b []byte) bool {
	return len(b) >= 2 && binary.LittleEndian.Uint16(b) == gzipMagic
}

func isZstd(b []byte) bool {
	return len(b) >= 4 && binary.LittleEndian.Uint32(b) == zstdMagic
}

func isZstdSkippable(b []byte) bool {
	// Zstd skippable frames use magic 0x184D2A50 through 0x184D2A5F.
	return len(b) >= 4 && binary.LittleEndian.Uint32(b)&0xFFFFFFF0 == zstdSkippableMagic
}

// loadZstdParallel decompresses a multi-frame zstd file using the
// embedded frame index for parallel decoding. The file format is:
//
//	[skippable frame: Magic_Number(4) + Frame_Size(4) +
//	  Index_Version(4) + N * (Compressed_Size(4) + Uncompressed_Size(4))]
//	[zstd frame 0]
//	[zstd frame 1]
//	...
func loadZstdParallel(ctx context.Context, buf []byte, threads int) ([]byte, error) {
	// Parse header and index data.
	if len(buf) < 12 {
		return nil, fmt.Errorf("buffer too short (%d bytes)", len(buf))
	}
	i := 0

	if magicNumber := binary.LittleEndian.Uint32(buf[i : i+4]); magicNumber != zstdSkippableMagic {
		return nil, fmt.Errorf("wrong zstd magic %x", magicNumber)
	}
	i += 4

	frameSize := int(binary.LittleEndian.Uint32(buf[i : i+4]))
	if frameSize < 4 || (frameSize-4)%8 != 0 {
		return nil, fmt.Errorf("invalid frame size %d", frameSize)
	}
	if len(buf) < 8+frameSize {
		return nil, fmt.Errorf("buffer too short for frame (%d bytes, need %d)", len(buf), 8+frameSize)
	}
	i += 4

	if indexVersion := binary.LittleEndian.Uint32(buf[i : i+4]); indexVersion != zstdIndexVersion {
		return nil, fmt.Errorf("wrong index version %x", indexVersion)
	}
	i += 4

	framesMetadata := buf[i : i+frameSize-4]
	numFrames := len(framesMetadata) / 8
	i += len(framesMetadata)

	framesData := buf[i:]

	// Build frame slices and output offsets.
	type frameInfo struct {
		data   []byte
		offset int
		size   int
	}
	frames := make([]frameInfo, numFrames)
	compOffset := 0
	outOffset := 0
	for frameIdx := range numFrames {
		compSize := int(binary.LittleEndian.Uint32(framesMetadata[frameIdx*8 : frameIdx*8+4]))
		outSize := int(binary.LittleEndian.Uint32(framesMetadata[frameIdx*8+4 : frameIdx*8+8]))
		if compOffset+compSize > len(framesData) {
			return nil, fmt.Errorf("frame %d extends beyond data (offset %d + size %d > %d)", frameIdx, compOffset, compSize, len(framesData))
		}
		frames[frameIdx] = frameInfo{
			data:   framesData[compOffset : compOffset+compSize],
			offset: outOffset,
			size:   outSize,
		}
		compOffset += compSize
		outOffset += outSize
	}
	totalUncompressed := outOffset

	clog.Infof(ctx, "parallel decode of %d frames (%d bytes) using %d threads", numFrames, totalUncompressed, threads)

	// Pre-allocate the output buffer.
	out := make([]byte, totalUncompressed)

	// DecodeAll is concurrency-safe: the decoder manages an internal pool
	// of block decoders sized by WithDecoderConcurrency.
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(threads))
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	// Decode each frame in parallel, directly into its slice of out.
	var eg errgroup.Group
	eg.SetLimit(threads)
	for _, f := range frames {
		eg.Go(func() error {
			dst := out[f.offset : f.offset : f.offset+f.size]
			result, err := dec.DecodeAll(f.data, dst)
			if err != nil {
				return err
			}
			if len(result) != f.size {
				return fmt.Errorf("frame decoded %d bytes, expected %d", len(result), f.size)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return out, nil
}

func loadFile(ctx context.Context, opts Option) ([]byte, error) {
	var compressed []byte
	if opts.UseMmap {
		// Use mmap to read the compressed file. The data is backed by the
		// OS page cache rather than the Go heap, avoiding a large allocation.
		data, err := mmapfile.Read(opts.StateFile)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("file %s is empty", opts.StateFile)
		}
		defer mmapfile.Unmap(data)
		compressed = data
	} else {
		data, err := os.ReadFile(opts.StateFile)
		if err != nil {
			return nil, err
		}
		compressed = data
	}

	if len(compressed) < 4 {
		return nil, errors.New("state file too short to determine compression format")
	}

	compressThreads := opts.CompressThreads
	if compressThreads == 0 {
		compressThreads = defaultCompressThreads
	}

	// Detect format by magic bytes.
	if isZstd(compressed) || isZstdSkippable(compressed) {
		// Our indexed chunked format prepends an empty zstd frame so that
		// older Siso versions that only check the first four bytes for
		// zstdMagic (0xFD2FB528) correctly identify the file as zstd and
		// decompress it using the standard sequential decoder.
		clog.Infof(ctx, "fs_state is zstd compressed (%d bytes)", len(compressed))
		compressed = bytes.TrimPrefix(compressed, zstdEmptyFrame)

		// If we find a zstd skippable frame next, we attempt to decode it
		// using our parallel decoding method. In case that fails (e.g.
		// incompatible future index format), we fall back to sequential
		// decoding - the file is still valid multi-frame zstd.
		if isZstdSkippable(compressed) {
			result, err := loadZstdParallel(ctx, compressed, compressThreads)
			if err == nil {
				return result, nil
			}
			clog.Warningf(ctx, "parallel zstd decode failed, falling back to sequential: %v", err)
		}

		// Standard sequential zstd decoding is always supported.
		dec, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		return dec.DecodeAll(compressed, nil)
	}
	if isGzip(compressed) {
		// Backward compatibility: decompress old gzip/bgzf state files.
		// Go's gzip.Reader handles both plain gzip and bgzf (concatenated gzip members).
		clog.Infof(ctx, "fs_state is gzip compressed (legacy format, %d bytes)", len(compressed))
		r, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	}

	return nil, errors.New("unknown compression format, neither gzip nor zstd")
}

// Load loads a HashFS's state.
func Load(ctx context.Context, opts Option) (*pb.State, error) {
	defer trace.Begin(ctx, "hashfs.Load").End()
	start := time.Now()
	b, err := loadFile(ctx, opts)
	if err != nil {
		return nil, err
	}
	durUncompress := time.Since(start)

	start = time.Now()
	state := &pb.State{}
	err = proto.Unmarshal(b, state)
	if err != nil {
		return nil, err
	}
	durUnmarshal := time.Since(start)
	clog.Infof(ctx, "Load fs state from %s: read/uncompress %s + unmarshal %s = total %s", opts.StateFile, durUncompress, durUnmarshal, durUncompress+durUnmarshal)

	return state, nil
}

type entryStateType int

const (
	entryNoLocal entryStateType = iota
	entryBeforeLocal
	entryEqLocal
	entryAfterLocal
)

func (et entryStateType) String() string {
	switch et {
	case entryNoLocal:
		return "no-local"
	case entryBeforeLocal:
		return "entry-before-local"
	case entryEqLocal:
		return "entry-eq-local"
	case entryAfterLocal:
		return "entry-after-local"
	default:
		return fmt.Sprintf("entryStateType=%d", int(et))
	}
}

func toDigest(d *pb.Digest) digest.Digest {
	if d == nil {
		return digest.Digest{}
	}
	return digest.Digest{
		Hash:      d.Hash,
		SizeBytes: d.SizeBytes,
	}
}

func fromDigest(d digest.Digest) *pb.Digest {
	if d.IsZero() {
		return nil
	}
	return &pb.Digest{
		Hash:      d.Hash,
		SizeBytes: d.SizeBytes,
	}
}

// entryState is used to convert *pb.Entry,local disk to *entry.
type entryState struct {
	ent *pb.Entry      // file entry state at the last build.
	et  entryStateType // indicate mtime difference from local disk

	ftype string // valid if "dir","symlink" or "file"
	e     entry  // file entry data

	prevGenerated bool // if file is generated output of build step.
	tainted       bool // if file is generated, but modified locally.
}

// initialEntryStates initializes file entries from *pb.State and local disk.
type initialEntryStates struct {
	alloc []entryState

	missingDigests []string // missing digests in *pb.State
	missingOutputs []string // missing outputs in *pb.State

	// interfaces to be used during initialization
	fsm         FileInfoer
	ignore      IgnoreFunc
	outputLocal OutputLocalFunc
	dataSource  DataSource
	osfs        *osfs.OSFS

	keepTainted bool

	// counters
	neq         atomic.Int64
	nnew        atomic.Int64
	nnotexist   atomic.Int64
	nfail       atomic.Int64
	ninvalidate atomic.Int64

	dirty atomic.Bool
}

// prepare prepares alloc/missingDigests/missingOutputs from state.
func (ies *initialEntryStates) prepare(ctx context.Context, state *pb.State) {
	started := time.Now()
	ies.missingDigests = state.MissingDigests
	ies.missingOutputs = state.MissingOutputs
	ndirs := 0
	ies.alloc = make([]entryState, len(state.Entries))

	for i, ent := range state.Entries {
		s := &ies.alloc[i]
		if runtime.GOOS == "windows" {
			ent.Name = strings.TrimPrefix(ent.Name, `\`)
		}
		ent.Name = filepath.ToSlash(ent.Name)
		s.ent = ent
		if ies.ignore(ctx, ent.Name) {
			clog.Infof(ctx, "ignore %q", ent.Name)
			continue
		}

		if ent.Digest == nil && ent.Target == "" {
			// directory
			ndirs++
		}
	}
	clog.Infof(ctx, "initial entryState init %d (dirs=%d) %s", len(state.Entries), ndirs, time.Since(started))
}

// updateFromDisk updates file entries from *pb.State and local disk
// and returns previouslyGeneratedFiles and taintedFiles.
func (ies *initialEntryStates) updateFromDisk(ctx context.Context) ([]string, []string, error) {
	started := time.Now()
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(runtime.GOMAXPROCS(0))
	for i := range ies.alloc {
		eg.Go(func() error {
			if i%1000 == 0 {
				select {
				case <-ctx.Done():
					err := context.Cause(ctx)
					return err
				default:
				}
			}
			es := &ies.alloc[i]
			return ies.updateEntryFromDisk(ctx, es)
		})
	}
	err := eg.Wait()
	if err != nil {
		return nil, nil, err
	}
	var previouslyGeneratedFiles, taintedFiles []string
	for i := range ies.alloc {
		es := &ies.alloc[i]
		if es.prevGenerated {
			previouslyGeneratedFiles = append(previouslyGeneratedFiles, es.ent.Name)
		}
		if es.tainted {
			taintedFiles = append(taintedFiles, es.ent.Name)
		}
	}
	clog.Infof(ctx, "update from disk %s", time.Since(started))
	return previouslyGeneratedFiles, taintedFiles, nil

}

// updateEntryFromDisk updates entry in es from *pb.Entry and local disk.
func (ies *initialEntryStates) updateEntryFromDisk(ctx context.Context, es *entryState) error {
	fi, err := ies.fsm.FileInfo(ctx, es.ent)
	if errors.Is(err, fs.ErrNotExist) {
		ies.initNotExist(ctx, es)
		return nil
	}
	if err != nil {
		ies.initErr(ctx, es, err)
		return nil
	}
	err = waitUntilModTime(ctx, es.ent.Name, fi.ModTime())
	if err != nil {
		return err
	}
	ies.initStateEntry(ctx, es, fi.ModTime())

	switch es.ftype {
	case "dir":
		if !ies.initDir(ctx, es, fi) {
			return nil
		}
	case "symlink":
		if !ies.initSymlink(ctx, es) {
			return nil
		}
	case "file":
		if !ies.initFile(ctx, es, fi) {
			return nil
		}
	}
	return ies.handleModTime(ctx, es, fi)
}

// initNotExist initializes an entry for file that doesn't exist on local disk.
func (ies *initialEntryStates) initNotExist(ctx context.Context, es *entryState) {
	if log.V(1) {
		clog.Infof(ctx, "not exist %q", es.ent.Name)
	}
	if len(es.ent.CmdHash) == 0 {
		es.e.err = fs.ErrNotExist
		ies.nnotexist.Add(1)
		ies.dirty.Store(true)
		clog.Infof(ctx, "not exist with no cmd hash: %q", es.ent.Name)
		return
	}
	// recorded as output file, but missing on disk.
	if es.ent.Local || ies.outputLocal(ctx, es.ent.Name) {
		// command output file that is needed on the disk doesn't exist on the disk.
		// need to forget to trigger steps for the output. b/298523549
		es.e.err = fs.ErrNotExist
		ies.nnotexist.Add(1)
		ies.dirty.Store(true)
		clog.Warningf(ctx, "not exist output-needed file: %q", es.ent.Name)
		return
	}
	// remote generated file, build without bytes.
	ies.initStateEntry(ctx, es, time.Time{})
}

// initErr initializes an entry for file with err.
func (ies *initialEntryStates) initErr(ctx context.Context, es *entryState, err error) {
	es.e.err = err
	clog.Warningf(ctx, "failed to stat %q: %v", es.ent.Name, err)
	ies.nfail.Add(1)
	ies.dirty.Store(true)
}

// initStateEntry initializes es.{et,ftype,e} with ftime.
// if ftime is zero, it doesn't exist on local disk.
func (ies *initialEntryStates) initStateEntry(ctx context.Context, es *entryState, ftime time.Time) {
	lready := make(chan bool, 1)
	entTime := time.Unix(0, es.ent.Id.ModTime)
	switch {
	case ftime.IsZero():
		// local doesn't exist
		es.et = entryNoLocal
		lready <- true
	case entTime.Before(ftime):
		es.et = entryBeforeLocal
		close(lready)
	case entTime.Equal(ftime):
		es.et = entryEqLocal
		close(lready)
	case entTime.After(ftime):
		es.et = entryAfterLocal
		lready <- true
	}
	mode := fs.FileMode(0644)
	if es.ent.IsExecutable {
		mode |= 0111
	}
	var dir *directory
	var src digest.Source
	entDigest := toDigest(es.ent.Digest)
	if !entDigest.IsZero() {
		es.ftype = "file"
		// regular file
		if es.et == entryEqLocal {
			src = ies.osfs.FileSource(es.ent.Name, entDigest.SizeBytes)
		} else {
			// not the same as local, but digest is in state.
			// probably, eixsts in RBE side, or local cache.
			src = ies.dataSource.Source(ctx, entDigest, es.ent.Name)
		}
	} else if es.ent.Target != "" {
		es.ftype = "symlink"
		// symlink
		mode |= fs.ModeSymlink
	} else {
		es.ftype = "dir"
		// Only dir entries need a child map.
		dir = &directory{}
		mode |= fs.ModeDir
	}
	updatedTime := time.Unix(0, es.ent.UpdatedTime)
	if updatedTime.Before(entTime) {
		updatedTime = entTime
	}
	es.e.lready = lready
	es.e.size = entDigest.SizeBytes
	es.e.mtime = entTime
	es.e.mode = mode
	es.e.updatedTime = updatedTime
	es.e.target = es.ent.Target
	es.e.src = src
	es.e.d = entDigest
	es.e.directory = dir

	es.e.cmdhash = es.ent.CmdHash
	es.e.edgehash = es.ent.EdgeHash
	es.e.action = toDigest(es.ent.Action)
	es.e.local = es.ent.Local

	es.prevGenerated = len(es.e.cmdhash) > 0
}

// initDir initializes es as dir.
func (ies *initialEntryStates) initDir(ctx context.Context, es *entryState, fi fs.FileInfo) bool {
	if !fi.IsDir() {
		es.ftype = ""
		clog.Warningf(ctx, "entry is dir, but local is not dir %q: mode=%s", es.ent.Name, fi.Mode())
		ies.nfail.Add(1)
		ies.dirty.Store(true)
		return false
	}
	if !es.prevGenerated { // len(es.e.cmdhash) == 0
		es.ftype = ""
		clog.Infof(ctx, "ignore dir %q: no cmd hash", es.ent.Name)
		return false
	}
	return true
}

// initSymlink initializes es as symlink.
func (ies *initialEntryStates) initSymlink(ctx context.Context, es *entryState) bool {
	t, err := os.Readlink(es.ent.Name)
	if err != nil {
		es.ftype = ""
		clog.Warningf(ctx, "failed to readlink %q: %v", es.ent.Name, err)
		ies.nfail.Add(1)
		ies.dirty.Store(true)
		return false
	}
	if t != es.e.target {
		es.ftype = ""
		clog.Warningf(ctx, "invalidate symlink %q: target:%q->%q", es.ent.Name, es.e.target, t)
		ies.ninvalidate.Add(1)
		ies.dirty.Store(true)
		return false
	}
	// symlink matches, make entry equals local
	if es.et != entryEqLocal {
		if log.V(1) {
			clog.Warningf(ctx, "symlnk target match %q: state=%v->%v", es.ent.Name, es.et, entryEqLocal)
		}
		es.et = entryEqLocal
	}
	return true
}

// initFile initializes as a file.
func (ies *initialEntryStates) initFile(ctx context.Context, es *entryState, fi fs.FileInfo) bool {
	if es.prevGenerated && es.et != entryEqLocal && !ies.dirty.Load() {
		// mtime differ for generated file?
		// check digest is the same and fix mtime if it matches.
		// don't reconcile for source (non-generated file),
		// as user may want to trigger build by touch.
		src := ies.osfs.FileSource(es.ent.Name, fi.Size())
		data, err := localDigest(ctx, src, es.ent.Name)
		if err == nil && data.Digest() == es.e.d {
			es.et = entryEqLocal
			err = ies.osfs.Chtimes(ctx, es.ent.Name, time.Now(), es.e.mtime)
			clog.Infof(ctx, "reconcile mtime %q %v -> %v: %v", es.ent.Name, fi.ModTime(), es.e.mtime, err)
		} else {
			clog.Warningf(ctx, "failed to reconcile mtime %q digest %s(stat) != %s(local) err: %v", es.ent.Name, es.e.d, data.Digest(), err)
		}
	}
	return true
}

// handleModTime handles modtime difference between *pb.Entry and fi (local disk).
func (ies *initialEntryStates) handleModTime(ctx context.Context, es *entryState, fi fs.FileInfo) error {
	switch es.et {
	case entryNoLocal:
		// it should not happen since we already checked it.
		return ies.handleNoLocal(ctx, es)

	case entryBeforeLocal:
		return ies.handleBeforeLocal(ctx, es, fi)
	case entryEqLocal:
		return ies.handleEqLocal(ctx, es)
	case entryAfterLocal:
		return ies.handleAfterLocal(ctx, es, fi)
	}
	return fmt.Errorf("invalid entryStateType: %s", es.et)
}

// handleNoLocal handles for entryNoLocal.
func (ies *initialEntryStates) handleNoLocal(ctx context.Context, es *entryState) error {
	ies.initNotExist(ctx, es)
	return nil
}

// handleBeforeLocal handles for entryBeforeLocal.
// i.e. *pb.Entry is older than local disk. local disk may be modified since last build.
func (ies *initialEntryStates) handleBeforeLocal(ctx context.Context, es *entryState, fi fs.FileInfo) error {
	ies.ninvalidate.Add(1)
	ies.dirty.Store(true)
	if !es.prevGenerated || !ies.keepTainted {
		es.ftype = ""
		clog.Warningf(ctx, "invalidate %s %q: state:%s disk:%s", es.ftype, es.ent.Name, es.e.mtime, fi.ModTime())
		return nil
	}
	es.tainted = true
	clog.Warningf(ctx, "keep tainted %s %q: state:%s disk:%s", es.ftype, es.ent.Name, es.e.mtime, fi.ModTime())
	// keep this entry to preserve cmdhash
	// but use mtime of actual file.
	es.e.mtime = fi.ModTime()
	return nil
}

// handleEqLocal handles for entryEqLocal.
// i.e. *pb.Entry matches with local disk. no modification since last build.
func (ies *initialEntryStates) handleEqLocal(ctx context.Context, es *entryState) error {
	ies.neq.Add(1)
	if log.V(1) {
		clog.Infof(ctx, "equal local %s %q: %s", es.ftype, es.ent.Name, es.e.mtime)
	}
	return nil
}

// handleAfterLocal handles for entryAfterLocal.
// i.e. *pb.Entry is newer than local disk.
func (ies *initialEntryStates) handleAfterLocal(ctx context.Context, es *entryState, fi fs.FileInfo) error {
	ies.nnew.Add(1)
	ies.dirty.Store(true)
	if !es.prevGenerated {
		es.ftype = ""
		clog.Warningf(ctx, "old local source %s %q: state:%s disk:%s", es.ftype, es.ent.Name, es.e.mtime, fi.ModTime())
		return nil
	}
	isOutputLocal := ies.outputLocal(ctx, es.ent.Name)
	clog.Infof(ctx, "old local %s %q: state:%s disk:%s cmdhash:%s outputLocal:%t", es.ftype, es.ent.Name, es.e.mtime, fi.ModTime(), base64.StdEncoding.EncodeToString(es.e.cmdhash), isOutputLocal)
	if isOutputLocal {
		// output file that is needed on the disk is stale.
		// need to forget to trigger steps for the output. b/418221857
		es.ftype = ""
		ies.ninvalidate.Add(1)
		return nil
	}
	// local file would be stale.
	// TODO: flush instead of removing local?
	err := os.Remove(es.ent.Name)
	if err != nil {
		clog.Warningf(ctx, "failed to remove stale old local file %q: %v", es.ent.Name, err)
		es.ftype = ""
		ies.ninvalidate.Add(1)
		return nil
	}
	// keep remote entry
	return nil
}

// clean reports whether state is clean or not.
func (ies *initialEntryStates) clean() bool {
	// missing outputs just makes dirty, but no need to set it in hfs.missingOutputs b/374179435
	return ies.nnew.Load() == 0 && ies.nnotexist.Load() == 0 && ies.nfail.Load() == 0 && ies.ninvalidate.Load() == 0 && len(ies.missingOutputs) == 0 && len(ies.missingDigests) == 0
}

// storeEntries stores every entry for which keep(ftype) is true, splitting
// ies.alloc into one chunk per worker.
func (ies *initialEntryStates) storeEntries(ctx context.Context, hfs *HashFS, keep func(ftype string) bool) error {
	eg, ctx := errgroup.WithContext(ctx)
	n := len(ies.alloc)
	nworkers := min(runtime.GOMAXPROCS(0), n)
	if nworkers < 1 {
		return nil
	}
	chunk := (n + nworkers - 1) / nworkers
	for start := 0; start < n; start += chunk {
		end := min(start+chunk, n)
		eg.Go(func() error {
			for i := start; i < end; i++ {
				es := &ies.alloc[i]
				if !keep(es.ftype) {
					continue
				}
				if _, err := hfs.directory.store(ctx, es.ent.Name, &es.e); err != nil {
					return fmt.Errorf("failed to store %s %q: %w", es.ftype, es.ent.Name, err)
				}
			}
			return nil
		})
	}
	return eg.Wait()
}

// storeDirs stores dir entries in hfs.
func (ies *initialEntryStates) storeDirs(ctx context.Context, hfs *HashFS) error {
	return ies.storeEntries(ctx, hfs, func(ftype string) bool { return ftype == "dir" })
}

// storeNonDirs stores non-dir entries (files, symlinks) in hfs.
func (ies *initialEntryStates) storeNonDirs(ctx context.Context, hfs *HashFS) error {
	return ies.storeEntries(ctx, hfs, func(ftype string) bool {
		return ftype == "symlink" || ftype == "file"
	})
}

// triggerDigestCalculation triggers digest calculation for missing digest files.
func (ies *initialEntryStates) triggerDigestCalculation(ctx context.Context, hfs *HashFS) {
	start := time.Now()
	for _, fname := range ies.missingDigests {
		hfs.Stat(ctx, "", fname) // access and trigger lazy digest calculation
	}
	clog.Infof(ctx, "set missing_digests=%d: %s", len(ies.missingDigests), time.Since(start))
}

// info returns info of the entryStates initialization.
func (ies *initialEntryStates) info() string {
	return fmt.Sprintf("eq:%d new:%d not-exist:%d fail:%d invalidate:%d missingOutputs:%d missingDigests:%d",
		ies.neq.Load(),
		ies.nnew.Load(),
		ies.nnotexist.Load(),
		ies.nfail.Load(),
		ies.ninvalidate.Load(),
		len(ies.missingOutputs),
		len(ies.missingDigests))
}

// SetState sets states to the HashFS.
func (hfs *HashFS) SetState(ctx context.Context, state *pb.State) error {
	defer trace.Begin(ctx, "hashfs.SetState").End()
	start := time.Now()

	octx := ctx // preserve original ctx
	logw := hfs.opt.SetStateLogger
	if logw != nil {
		// not show this for `siso fs state`, but for e2etest
		if logw != os.Stdout {
			fmt.Fprintf(logw, "hashfs.SetState\n")
			defer fmt.Fprintf(logw, "hashfs.SetState done\n")
		}
		ctx = clog.NewContext(ctx, clog.FromContext(ctx).WithWriter(logw))
	}

	if state.BuildTargets != nil {
		hfs.buildTargets = make([]string, len(state.BuildTargets.Targets))
		copy(hfs.buildTargets, state.BuildTargets.Targets)
		clog.Infof(ctx, "build targets=%q", hfs.buildTargets)
	} else {
		hfs.buildTargets = nil
		clog.Infof(ctx, "no build targets")
	}
	var fsm FileInfoer = osfsInfoer{}
	if hfs.opt.FSMonitor != nil && state.LastChecked != "" {
		f, err := hfs.opt.FSMonitor.Scan(ctx, state.LastChecked)
		if err != nil {
			clog.Warningf(ctx, "failed to fsmonitor scan %q: %v", state.LastChecked, err)
		} else {
			clog.Infof(ctx, "use fsmonitor scan %q", state.LastChecked)
			if logw != nil {
				fmt.Fprintf(logw, "use fsmonitor scan %q\n", state.LastChecked)
			}
			fsm = f
		}
	}
	initial := new(initialEntryStates)
	initial.fsm = fsm
	initial.ignore = hfs.opt.Ignore
	initial.outputLocal = hfs.opt.OutputLocal
	initial.dataSource = hfs.opt.DataSource
	initial.osfs = hfs.OS
	initial.keepTainted = hfs.opt.KeepTainted

	initial.prepare(ctx, state)

	var err error
	hfs.previouslyGeneratedFiles, hfs.taintedFiles, err = initial.updateFromDisk(ctx)
	if err != nil {
		clog.Warningf(ctx, "failed in SetState updateFromDisk: %v", err)
		return err
	}

	hfs.setStateCh = make(chan error, 1)

	clean := initial.clean()
	hfs.clean.Store(clean)
	// store in background.
	go func() {
		ctx := octx // use ctx without logw.
		ctx = trace.NewThread(ctx, "hashfs.SetState.store")
		defer trace.Begin(ctx, "hashfs.SetState.store").End()
		defer close(hfs.setStateCh)
		// name is sorted in state.Entries.

		// store dir early.
		// otherwise, flaky confirm no-op failure
		// for step that outputs dir and dir/file.
		// i.e. if dir/file is stored before dir,
		// dir/file's cmdhash etc will be lost.
		err := initial.storeDirs(ctx, hfs)
		if err != nil {
			hfs.clean.Store(false)
			hfs.setStateCh <- err
			return
		}
		err = initial.storeNonDirs(ctx, hfs)
		if err != nil {
			hfs.clean.Store(false)
			hfs.setStateCh <- err
			return
		}
		hfs.loaded.Store(true)
		clog.Infof(ctx, "set state done: clean:%t loaded:true: %s", hfs.clean.Load(), time.Since(start))
		if hfs.opt.DeferDigest {
			clog.Infof(ctx, "deferred stat missing_digests=%d", len(state.MissingDigests))
		} else {
			initial.triggerDigestCalculation(ctx, hfs)
		}
		hfs.setStateCh <- nil
	}()
	clog.Infof(ctx, "load state done: %s tainted:%d prevGenerated:%d %s", initial.info(), len(hfs.taintedFiles), len(hfs.previouslyGeneratedFiles), time.Since(start))
	return nil
}

// zstdCompressor handles parallel zstd compression of data into multiple
// independent frames. It pre-computes the worst-case output size so the
// caller can allocate the output buffer (e.g. via mmap) before compressing.
type zstdCompressor struct {
	enc       *zstd.Encoder
	threads   int
	dataLen   int
	chunkSize int
	numFrames int
}

// newZstdCompressor creates a compressor for the given data length.
// Call Close when done.
func newZstdCompressor(dataLen int, level zstd.EncoderLevel, threads int) (*zstdCompressor, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(level),
		zstd.WithEncoderConcurrency(threads),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		return nil, err
	}
	chunkSize := max(1, (dataLen+threads-1)/threads)
	numFrames := (dataLen + chunkSize - 1) / chunkSize
	return &zstdCompressor{
		enc:       enc,
		threads:   threads,
		dataLen:   dataLen,
		chunkSize: chunkSize,
		numFrames: numFrames,
	}, nil
}

func (c *zstdCompressor) Close() error { return c.enc.Close() }

// MaxCompressedSize returns the worst-case total output size (empty frame +
// skippable index frame + all data frames). Use this to size the output buffer.
func (c *zstdCompressor) MaxCompressedSize() int {
	indexPayload := 4 + c.numFrames*8
	total := len(zstdEmptyFrame) + 8 + indexPayload
	for i := range c.numFrames {
		start := i * c.chunkSize
		end := min(start+c.chunkSize, c.dataLen)
		total += c.enc.MaxEncodedSize(end - start)
	}
	return total
}

// Compress compresses data into out and returns the number of bytes
// written. out must be at least MaxCompressedSize() bytes.
//
// The output file format is:
//
//	[zstd empty frame (9 bytes)]
//	[skippable frame: Magic_Number(4) + Frame_Size(4) +
//	  Index_Version(4) + N * (Compressed_Size(4) + Uncompressed_Size(4))]
//	[zstd frame 0]
//	[zstd frame 1]
//	...
//
// The leading empty zstd frame ensures older Siso versions that only
// check for zstdMagic (0xFD2FB528) in the first four bytes correctly
// identify the file as zstd-compressed and fall back to sequential
// decoding.
func (c *zstdCompressor) Compress(out, data []byte) (int, error) {
	// Assign worst-case offsets within out for each frame.
	indexPayload := 4 + c.numFrames*8
	headerSize := len(zstdEmptyFrame) + 8 + indexPayload
	type frameSlot struct {
		dataStart  int // offset in out where compressed data begins
		maxSize    int // worst-case compressed size for this frame
		compSize   int // actual compressed size (filled after compression)
		uncompSize int // uncompressed size of the input chunk
	}
	slots := make([]frameSlot, c.numFrames)
	offset := headerSize
	var eg errgroup.Group
	eg.SetLimit(c.threads)
	for i := range c.numFrames {
		start := i * c.chunkSize
		end := min(start+c.chunkSize, len(data))
		uncompSize := end - start
		maxSize := c.enc.MaxEncodedSize(uncompSize)
		slots[i] = frameSlot{
			dataStart:  offset,
			maxSize:    maxSize,
			uncompSize: uncompSize,
		}
		chunk := data[start:end]
		slot := &slots[i]
		eg.Go(func() error {
			dst := out[slot.dataStart : slot.dataStart : slot.dataStart+slot.maxSize]
			result := c.enc.EncodeAll(chunk, dst)
			slot.compSize = len(result)
			return nil
		})
		offset += maxSize
	}
	if err := eg.Wait(); err != nil {
		return 0, err
	}

	// Compact: move each frame's actual data to be contiguous (removing
	// the gaps from worst-case over-estimation).
	writePos := headerSize
	for i := range slots {
		s := &slots[i]
		if writePos != s.dataStart {
			copy(out[writePos:], out[s.dataStart:s.dataStart+s.compSize])
		}
		writePos += s.compSize
	}

	// Write the empty frame + skippable frame header + index at the beginning.
	pos := copy(out, zstdEmptyFrame)
	binary.LittleEndian.PutUint32(out[pos:], zstdSkippableMagic)
	pos += 4
	binary.LittleEndian.PutUint32(out[pos:], uint32(indexPayload))
	pos += 4
	binary.LittleEndian.PutUint32(out[pos:], zstdIndexVersion)
	pos += 4
	for _, s := range slots {
		binary.LittleEndian.PutUint32(out[pos:], uint32(s.compSize))
		pos += 4
		binary.LittleEndian.PutUint32(out[pos:], uint32(s.uncompSize))
		pos += 4
	}

	return writePos, nil
}

func saveFile(ctx context.Context, data []byte, opts Option) (retErr error) {
	compressThreads := opts.CompressThreads
	if compressThreads == 0 {
		compressThreads = defaultCompressThreads
	}

	level := zstd.EncoderLevelFromZstd(opts.CompressLevel)
	clog.Infof(ctx, "compressing fs_state with zstd (level %d, %d threads, %d bytes)", opts.CompressLevel, compressThreads, len(data))

	comp, err := newZstdCompressor(len(data), level, compressThreads)
	if err != nil {
		return err
	}
	defer func() {
		if err := comp.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()

	f, err := os.CreateTemp(filepath.Dir(opts.StateFile), filepath.Base(opts.StateFile)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(f.Name())
		}
	}()

	if opts.UseMmap {
		// Mmap the temp file at worst-case size and compress directly
		// into the mmap'd region, avoiding a heap-allocated output buffer.
		out, closer, err := mmapfile.Write(f, comp.MaxCompressedSize())
		if err != nil {
			_ = os.Remove(f.Name())
			return err
		}

		actualSize, err := comp.Compress(out, data)
		if err != nil {
			closer()
			return err
		}
		clog.Infof(ctx, "compressed %d -> %d bytes (%.1fx)", len(data), actualSize, float64(len(data))/float64(actualSize))

		if err := closer(); err != nil {
			return err
		}

		// Truncate from worst-case to actual compressed size.
		if err := os.Truncate(f.Name(), int64(actualSize)); err != nil {
			return err
		}
	} else {
		out := make([]byte, comp.MaxCompressedSize())
		actualSize, err := comp.Compress(out, data)
		if err != nil {
			return err
		}
		clog.Infof(ctx, "compressed %d -> %d bytes (%.1fx)", len(data), actualSize, float64(len(data))/float64(actualSize))

		if _, err := f.Write(out[:actualSize]); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	// save old state in *.0
	ofname := opts.StateFile + ".0"
	if err := os.Remove(ofname); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(opts.StateFile, ofname); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	err = os.Rename(f.Name(), opts.StateFile)
	clog.Infof(ctx, "replace %s: %v", opts.StateFile, err)
	return err
}

// Save persists state in fname.
func Save(ctx context.Context, state *pb.State, opts Option) error {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		// state is broken?? panic in proto.Marshal b/323265794
		// use glog, not cloud logging
		// because siso terminates before cloud logging entries
		// are uploaded.
		log.Errorf("state: %d entries", len(state.Entries))
		for i, ent := range state.Entries {
			err := func(ent *pb.Entry) (err error) {
				defer func() {
					r := recover()
					if r != nil {
						err = fmt.Errorf("panic in marshal: %v", r)
					}
				}()
				_, err = proto.Marshal(ent)
				return err
			}(ent)
			log.Errorf("entries[%d] = %v: %v", i, ent, err)
		}
		log.Flush()
		panic(r)
	}()
	start := time.Now()
	b, err := proto.Marshal(state)
	if err != nil {
		return err
	}
	durMarshal := time.Since(start)

	start = time.Now()
	err = saveFile(ctx, b, opts)
	if err != nil {
		return err
	}
	durSave := time.Since(start)

	clog.Infof(ctx, "Save fs state to %s: marshal %s + compress/save %s = total %s", opts.StateFile, durMarshal, durSave, durMarshal+durSave)

	// Journal data are already included in state.
	// Remove journal file as it is not needed to reconcile in next build.
	err = os.Remove(opts.StateFile + ".journal")
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// State returns a State of the HashFS.
func (hfs *HashFS) State(ctx context.Context) *pb.State {
	started := time.Now()
	state := &pb.State{}
	type d struct {
		name string
		dir  *directory
	}
	var dirs []d
	dirs = append(dirs, d{name: "/", dir: hfs.directory})
	for len(dirs) > 0 {
		dir := dirs[0]
		dirs = dirs[1:]
		var names []string
		if log.V(1) {
			clog.Infof(ctx, "state dir=%s dirs=%d", dir.name, len(dirs))
		}
		// TODO(b/254182269): need mutex here?
		dir.dir.m.Range(func(k, _ any) bool {
			name := filepath.ToSlash(filepath.Join(dir.name, k.(string)))
			names = append(names, name)
			return true
		})
		sort.Strings(names)
		if log.V(1) {
			clog.Infof(ctx, "state dir=%s -> %q", dir.name, names)
		}
		for _, name := range names {
			v, ok := dir.dir.m.Load(filepath.Base(name))
			if !ok {
				if dir.name == "/" && name == "/" {
					continue
				}
				clog.Errorf(ctx, "dir:%s name:%s entries:%v", dir.name, name, dir.dir)
				continue
			}
			e := v.(*entry)
			if e.err != nil {
				if bool(log.V(1)) || !errors.Is(e.err, fs.ErrNotExist) {
					clog.Infof(ctx, "ignore %s: err:%v", name, e.err)
				}
				continue
			}
			if runtime.GOOS == "windows" {
				name = strings.TrimPrefix(name, "/")
				if len(name) == 2 && name[1] == ':' {
					name += `/`
				}
			}
			if e.isDirectory() {
				// TODO(b/253541407): record mtime for other directory?
				dirs = append(dirs, d{name: name, dir: e.directory})
			}
			if e.mtime.IsZero() {
				if len(e.cmdhash) > 0 {
					clog.Warningf(ctx, "wrong entry for %s: mtime is zero, but cmdhash set %s", name, e.cmdhash)
				} else if log.V(1) {
					clog.Infof(ctx, "ignore %s: no mtime", name)
				}
				continue
			}
			// need to record the entry for incremental build
			ed := e.digest()
			if !e.isDirectory() && !e.isSymlink() && ed.IsZero() {
				// digest is not calculated yet?
				if e.src == nil {
					clog.Warningf(ctx, "wrong entry for %s?", name)
					state.MissingDigests = append(state.MissingDigests, name)
				} else if len(e.cmdhash) > 0 {
					clog.Warningf(ctx, "need to calculate digest for %s: cmdhash=%v", name, e.cmdhash)
					err := e.compute(ctx, name)
					if err != nil {
						clog.Warningf(ctx, "failed to calculate digest for %s: %v", name, err)
						state.MissingDigests = append(state.MissingDigests, name)
					} else {
						ed = e.digest()
						if ed.IsZero() {
							// e.compute failed to calculate digest?
							clog.Warningf(ctx, "compute returned nil-error, but failed to calculate digest for %s?", name)
							state.MissingDigests = append(state.MissingDigests, name)
						}
					}
				} else {
					if log.V(1) {
						clog.Warningf(ctx, "digest is unknown %s", name)
					}
					state.MissingDigests = append(state.MissingDigests, name)
				}
			}
			if !ed.IsZero() || e.isSymlink() || (!e.isDirectory() && len(e.cmdhash) > 0) {
				e.mu.RLock()
				state.Entries = append(state.Entries, &pb.Entry{
					Id: &pb.FileID{
						ModTime: e.mtime.UnixNano(),
					},
					Name:         name,
					Digest:       fromDigest(e.d),
					IsExecutable: e.mode&0111 != 0,
					Target:       e.target,
					CmdHash:      e.cmdhash,
					EdgeHash:     e.edgehash,
					Action:       fromDigest(e.action),
					Local:        e.local,
					UpdatedTime:  e.updatedTime.UnixNano(),
				})
				e.mu.RUnlock()
			} else if e.isDirectory() && len(e.cmdhash) > 0 {
				// preserve dir for cmdhash
				e.mu.RLock()
				state.Entries = append(state.Entries, &pb.Entry{
					Id: &pb.FileID{
						ModTime: e.mtime.UnixNano(),
					},
					Name:        name,
					CmdHash:     e.cmdhash,
					EdgeHash:    e.edgehash,
					Action:      fromDigest(e.action),
					Local:       e.local,
					UpdatedTime: e.updatedTime.UnixNano(),
				})
				e.mu.RUnlock()
			} else if len(e.cmdhash) > 0 {
				clog.Warningf(ctx, "wrong entry for %s: cmdhash is set, but no digest?", name)
			}
		}
	}
	if hfs.opt.FSMonitor != nil {
		token, err := hfs.opt.FSMonitor.ClockToken(ctx)
		if err != nil {
			clog.Warningf(ctx, "failed to get fsmonitor token: %v", err)
		} else {
			clog.Infof(ctx, "fsmonitor last checked = %q", token)
			state.LastChecked = token
		}
	}
	if hfs.buildTargets != nil {
		state.BuildTargets = &pb.BuildTargets{
			Targets: hfs.buildTargets,
		}
	}
	hfs.missingOutputs.Range(func(key, value any) bool {
		if fname, ok := key.(string); ok {
			state.MissingOutputs = append(state.MissingOutputs, fname)
		}
		return true
	})
	clog.Infof(ctx, "state %d entries token:%q buildTargets:%v: missingOutputs:%d missingDigests:%d %s", len(state.Entries), state.LastChecked, state.BuildTargets, len(state.MissingOutputs), len(state.MissingDigests), time.Since(started))
	return state
}

func StateMap(s *pb.State) map[string]*pb.Entry {
	m := make(map[string]*pb.Entry)
	for _, e := range s.Entries {
		m[e.Name] = e
	}
	return m
}

func loadJournal(ctx context.Context, fname string, state *pb.State) bool {
	started := time.Now()
	b, err := os.ReadFile(fname)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			clog.Infof(ctx, "no fs state journal: %v", err)
		} else {
			clog.Warningf(ctx, "Failed to load journal: %v", err)
		}
		return false
	}
	var cnt int
	var broken bool
	m := StateMap(state)
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		ent := &pb.Entry{}
		err := dec.Decode(&ent)
		if err != nil {
			clog.Warningf(ctx, "Failed to decode journal: %v", err)
			broken = true
			break
		}
		m[ent.Name] = ent
		if log.V(1) {
			clog.Infof(ctx, "from journal %s", ent.Name)
		}
		cnt++
	}
	if cnt == 0 {
		return false
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	state.Entries = make([]*pb.Entry, 0, len(keys))
	for _, k := range keys {
		state.Entries = append(state.Entries, m[k])
	}
	clog.Infof(ctx, "reconcile from journal %d entries (broken=%t) in %s", cnt, broken, time.Since(started))
	return true
}

func (hfs *HashFS) journalEntry(ctx context.Context, fname string, e *entry) {
	// If there are any tainted files, don't journal the entry.
	// This is because trainted files may have been modified by the user,
	// i.e. not by generated by the build from the source,
	// and we don't want to overwrite their changes with the state from
	// the journal.
	if len(hfs.taintedFiles) > 0 {
		return
	}
	if e.digest().IsZero() {
		hfs.digester.compute(ctx, fname, e)
	}
	e.mu.Lock()
	ent := &pb.Entry{
		Id: &pb.FileID{
			ModTime: e.mtime.UnixNano(),
		},
		Name:         fname,
		Digest:       fromDigest(e.d),
		IsExecutable: e.mode&0111 != 0,
		Target:       e.target,
		CmdHash:      e.cmdhash,
		EdgeHash:     e.edgehash,
		Action:       fromDigest(e.action),
		Local:        e.local,
		UpdatedTime:  e.updatedTime.UnixNano(),
	}
	e.mu.Unlock()
	err := JournalEntry(&hfs.journal, ent)
	if err != nil {
		clog.Warningf(ctx, "Failed to write journal entry %s: %v", fname, err)
	}
}

type journalWriter struct {
	mu sync.Mutex
	w  io.WriteCloser
	n  int
}

func (w *journalWriter) Write(buf []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.w == nil {
		return len(buf), nil
	}
	w.n++
	return w.w.Write(buf)
}

func (w *journalWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.w == nil {
		return nil
	}
	err := w.w.Close()
	w.w = nil
	return err
}

// JournalEntry writes ent in to journal writer w.
func JournalEntry(w io.Writer, ent *pb.Entry) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err := enc.Encode(ent)
	if err != nil {
		return err
	}
	_, err = w.Write(buf.Bytes())
	return err
}
