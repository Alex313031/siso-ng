// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"

	"google.golang.org/protobuf/types/known/anypb"

	"go.chromium.org/build/siso/execute"
	epb "go.chromium.org/build/siso/execute/proto"
)

// ServeSpawnHelper runs the helper side of the spawn protocol on the inherited
// socketpair fd, returning when the connection closes (siso exited) or ctx ends.
// logger receives the helper's diagnostics (its own log file, never siso's UI).
func ServeSpawnHelper(ctx context.Context, connFd int, logger *log.Logger) error {
	f := os.NewFile(uintptr(connFd), "spawn-helper-conn")
	conn, err := net.FileConn(f)
	// FileConn dups the fd (close-on-exec) and owns the copy; drop ours so
	// spawned children never inherit the control socket.
	f.Close()
	if err != nil {
		return fmt.Errorf("spawn helper: fileconn(fd %d): %w", connFd, err)
	}

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return fmt.Errorf("spawn helper: unexpected conn type %T", conn)
	}

	return serve(ctx, newSpawnConn(uc), logger)
}

func serve(ctx context.Context, conn *spawnConn, logger *log.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Non-fatal: without a subreaper, orphans reparent to init, which reaps
	// them on any normal (non-PID-1) host.
	if err := becomeSubreaper(); err != nil {
		logger.Printf("become child subreaper: %v", err)
	}

	// Unblock recv() if ctx is cancelled (e.g. SIGTERM).
	go func() {
		<-ctx.Done()
		_ = conn.close()
	}()

	var mu sync.Mutex
	inflight := make(map[uint64]context.CancelFunc)
	// wg tracks running actions so we drain them before returning (which ends the
	// process); otherwise cancelled children are orphaned and keep modifying outputs.
	var wg sync.WaitGroup

	for {
		msg, err := conn.recv()
		if err != nil {
			// EOF means siso (our parent) is gone, or ctx closed the conn:
			// cancel everything in flight, wait for it to terminate, then exit.
			mu.Lock()
			for _, cancelFunc := range inflight {
				cancelFunc()
			}
			mu.Unlock()
			wg.Wait()
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		switch msg.Payload.(type) {
		case *epb.SpawnMessage_Start:
			handleStart(ctx, conn, logger, msg.Id, msg.GetStart(), &mu, inflight, &wg)
		case *epb.SpawnMessage_Cancel:
			mu.Lock()
			if cancelFunc := inflight[msg.Id]; cancelFunc != nil {
				cancelFunc()
			}
			mu.Unlock()
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

func resultMsg(id uint64, actionResult *anypb.Any) *epb.SpawnMessage {
	return &epb.SpawnMessage{
		Id: id,
		Payload: &epb.SpawnMessage_Result{
			Result: &epb.SpawnResult{
				ActionResult: actionResult,
			},
		},
	}
}

// handleStart spawns one action in its own goroutine, reusing run() so the
// helper shares all spawn/wait/cancel/rusage/oom behavior with the direct path.
func handleStart(ctx context.Context, conn *spawnConn, logger *log.Logger, id uint64, req *epb.SpawnRequest, mu *sync.Mutex, inflight map[uint64]context.CancelFunc, wg *sync.WaitGroup) {
	// reply sends one helper->client message. A send failure means siso (our
	// parent) is gone: record it and close the conn so serve()'s recv() unblocks
	// and drains. The log goes to the helper's own file, never siso's progress UI.
	reply := func(msg *epb.SpawnMessage) {
		if err := conn.send(msg); err != nil {
			logger.Printf("send id=%d: %v", id, err)
			_ = conn.close()
		}
	}
	if len(req.GetArgs()) == 0 {
		reply(errorMsg(id, "empty args"))
		return
	}

	var oomScoreAdj *int
	if req.OomScoreAdj != nil {
		oomScoreAdj = new(int(req.GetOomScoreAdj()))
	}

	cctx, cancel := context.WithCancel(ctx)
	mu.Lock()
	inflight[id] = cancel
	mu.Unlock()

	// wg.Go counts this action (Add(1)) synchronously before launching, so
	// serve's drain (wg.Wait on exit) can't race ahead of a just-spawned action.
	wg.Go(func() {
		defer func() {
			mu.Lock()
			delete(inflight, id)
			mu.Unlock()
			cancel()
		}()

		cmd := &execute.Cmd{
			ID:            req.GetId(),
			Args:          req.GetArgs(),
			Env:           req.GetEnv(),
			WorkspaceRoot: req.GetWorkspaceRoot(),
			WorkDir:       req.GetWorkDir(),
			OOMScoreAdj:   oomScoreAdj,
		}

		res, runErr := run(cctx, cmd)
		if runErr != nil {
			reply(errorMsg(id, runErr.Error()))
			return
		}
		anyRes, err := anypb.New(res)
		if err != nil {
			reply(errorMsg(id, "pack result: "+err.Error()))
			return
		}
		reply(resultMsg(id, anyRes))
	})
}
