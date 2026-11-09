// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package spawnhelper

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"

	epb "go.chromium.org/build/siso/execute/proto"
)

// Spawner runs one action as a subprocess for the spawn helper server.
type Spawner interface {
	Spawn(context.Context, *epb.SpawnRequest) (*epb.SpawnResult, error)
}

// Server serves the spawn protocol on an inherited socketpair fd.
type Server struct {
	connFD       int
	logFile      string
	blockNetwork bool

	conn    *spawnConn
	spawner Spawner

	mu       sync.Mutex
	inflight map[uint64]context.CancelFunc
	wg       sync.WaitGroup
}

// RegisterFlags registers the server's command-line flags.
func (s *Server) RegisterFlags(fs *flag.FlagSet) {
	fs.IntVar(&s.connFD, "conn_fd", 0, "inherited socketpair fd to serve the spawn protocol on")
	fs.StringVar(&s.logFile, "log_file", "", "file for the helper's diagnostics (default: stderr)")
	fs.BoolVar(&s.blockNetwork, "block_network", false, "whether network is blocked")
}

// Serve reads spawn requests off the inherited socketpair fd and
// runs each one through spawner, until the connection closes (siso exited)
// or ctx ends.
// On exit, it cancels and drains any in-flight actions.
// It returns nil for a clean shutdown (EOF or ctx cancel) and
// an error otherwise.
func (s *Server) Serve(ctx context.Context, spawner Spawner) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.spawner = spawner

	if s.connFD <= 0 {
		return fmt.Errorf("-conn_fd is required")
	}

	f := os.NewFile(uintptr(s.connFD), "spawn-helper-conn")
	conn, err := net.FileConn(f)
	// FileConn dups the fd (close-on-exec) and owns the copy; drop ours so
	// spawned children never inherit the control socket.
	f.Close()
	if err != nil {
		return fmt.Errorf("spawn helper: fileconn(fd %d): %w", s.connFD, err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return fmt.Errorf("spawn helper: unexpected conn type %T", conn)
	}
	s.conn = newSpawnConn(uc)

	// Diagnostics go to the helper's own log file (siso_spawn_helper), so they
	// never clash with siso's stdout-based progress UI. Fall back to stderr when
	// no log file was given (e.g. under `go test`).
	logw := os.Stderr
	if s.logFile != "" {
		f, err := os.Create(s.logFile)
		if err != nil {
			return fmt.Errorf("spawn-helper: create log file %q: %w", s.logFile, err)
		}
		defer f.Close()
		logw = f
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetOutput(logw)
	// TODO: we'd like to plumb logging via clog as
	// localexec spawner may use clog for logging,
	// but clog doesn't support standard log.Logger as sink.

	s.inflight = make(map[uint64]context.CancelFunc)

	// Non-fatal: without a subreaper, orphans reparent to init, which reaps
	// them on any normal (non-PID-1) host.
	if err := becomeSubreaper(); err != nil {
		log.Printf("become child subreaper: %v", err)
	}

	// Unblock recv() if ctx is cancelled (e.g. SIGTERM).
	go func() {
		<-ctx.Done()
		_ = s.conn.close()
	}()

	if s.blockNetwork {
		if err := ifaceUp("lo"); err != nil {
			log.Printf("spawn helper: failed to bring up lo: %v", err)
		}
	}

	// wg tracks running actions so we drain them before returning (which ends the
	// process); otherwise cancelled children are orphaned and keep modifying outputs.
	for {
		msg, err := s.conn.recv()
		if err != nil {
			// EOF means siso (our parent) is gone, or ctx closed the conn:
			// cancel everything in flight, wait for it to terminate, then exit.
			s.mu.Lock()
			for _, cancelFunc := range s.inflight {
				cancelFunc()
			}
			s.mu.Unlock()
			s.wg.Wait()
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		switch msg.Payload.(type) {
		case *epb.SpawnMessage_Start:
			s.handleStart(ctx, msg.Id, msg.GetStart())
		case *epb.SpawnMessage_Cancel:
			s.mu.Lock()
			if cancelFunc := s.inflight[msg.Id]; cancelFunc != nil {
				cancelFunc()
			}
			s.mu.Unlock()
		}
	}
}

// errorMsg and resultMsg build the helper -> client reply for action id.
func errorMsg(id uint64, message string) *epb.SpawnMessage {
	return &epb.SpawnMessage{
		Id: id,
		Payload: &epb.SpawnMessage_Error{
			Error: &epb.SpawnError{
				Message: message,
			},
		},
	}
}

func resultMsg(id uint64, result *epb.SpawnResult) *epb.SpawnMessage {
	return &epb.SpawnMessage{
		Id: id,
		Payload: &epb.SpawnMessage_Result{
			Result: result,
		},
	}
}

// handleStart spawns one action in its own goroutine with Spawner.
func (s *Server) handleStart(ctx context.Context, id uint64, req *epb.SpawnRequest) {
	// reply sends one helper->client message. A send failure means siso (our
	// parent) is gone: record it and close the conn so serve()'s recv() unblocks
	// and drains. The log goes to the helper's own file, never siso's progress UI.
	reply := func(msg *epb.SpawnMessage) {
		if err := s.conn.send(msg); err != nil {
			log.Printf("send id=%d: %v", id, err)
			_ = s.conn.close()
		}
	}
	if len(req.GetArgs()) == 0 {
		reply(errorMsg(id, "empty args"))
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.inflight[id] = cancel
	s.mu.Unlock()

	// wg.Go counts this action (Add(1)) synchronously before launching, so
	// serve's drain (wg.Wait on exit) can't race ahead of a just-spawned action.
	s.wg.Go(func() {
		defer func() {
			s.mu.Lock()
			delete(s.inflight, id)
			s.mu.Unlock()
			cancel()
		}()

		res, err := s.spawner.Spawn(ctx, req)
		if err != nil {
			reply(errorMsg(id, err.Error()))
			return
		}
		if res == nil {
			reply(errorMsg(id, "spawner returned no result"))
			return
		}
		reply(resultMsg(id, res))
	})
}
