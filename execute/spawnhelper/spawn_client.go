// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package spawnhelper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	epb "go.chromium.org/build/siso/execute/proto"
)

// spawnReply is one demultiplexed reply for a pending Run: either
// a SpawnResult or an error (a SpawnError from the helper, or a transport
// failure).
type spawnReply struct {
	err    error
	result *epb.SpawnResult
}

// Client is the siso-side handle to a running spawn helper. It multiplexes
// concurrent actions over one socketpair connection, keyed by request id.
type Client struct {
	cmd  *exec.Cmd
	conn *spawnConn

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan spawnReply
	dead    error // set once the connection fails; later Runs fail fast
}

// launch executes args with `-conn_fd 3`, handing it one end of a unix
// socketpair. Run it while siso's heap is small.
// logFile, if non-empty, is forwarded so the helper writes its diagnostics there.
func Launch(args []string, logFile string, blockNetwork bool) (*Client, error) {
	// Hold ForkLock and set close-on-exec so a concurrent fork+exec doesn't leak
	// these fds; ExtraFiles re-clears CLOEXEC on the child's inherited copy.
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		syscall.ForkLock.RUnlock()
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	syscall.ForkLock.RUnlock()

	parent := os.NewFile(uintptr(fds[0]), "spawn-helper-parent")
	child := os.NewFile(uintptr(fds[1]), "spawn-helper-child")
	// Drop our copy of the child's end after starting the helper: if siso kept
	// it open, siso's own conn would never see EOF when the helper died.
	defer child.Close()

	args = append(args, "-conn_fd", "3")
	if logFile != "" {
		args = append(args, "-log_file", logFile)
	}
	if blockNetwork {
		args = append(args, "-block_network")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stderr // helper fatal/panic output goes to siso's stderr
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{child} // becomes fd 3 in the helper
	// Put the helper in its own process group so a terminal Ctrl-C or group
	// SIGTERM to siso doesn't kill it and its children before serve can drain.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := setNetworkPolicy(cmd, blockNetwork); err != nil {
		parent.Close()
		return nil, fmt.Errorf("network policy: %w", err)
	}
	if err := cmd.Start(); err != nil {
		parent.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	// net.FileConn dups the fd (close-on-exec) and owns the copy; drop ours.
	conn, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("fileconn: %w", err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("unexpected conn type %T", conn)
	}
	c := &Client{
		cmd:     cmd,
		conn:    newSpawnConn(uc),
		pending: make(map[uint64]chan spawnReply),
	}
	go c.readLoop()
	return c, nil
}

// readLoop demultiplexes reply messages to waiting Run calls until the connection
// fails (e.g. the helper exits), after which all pending and future Runs fail.
func (c *Client) readLoop() {
	for {
		msg, err := c.conn.recv()
		if err != nil {
			c.failAll(err)
			return
		}
		var reply spawnReply
		switch p := msg.Payload.(type) {
		case *epb.SpawnMessage_Result:
			reply.result = p.Result
		case *epb.SpawnMessage_Error:
			// Raw helper-side message; callers (e.g. runViaHelper) add the
			// "spawn helper:" context so it isn't duplicated.
			reply.err = errors.New(p.Error.GetMessage())
		default:
			continue
		}
		c.mu.Lock()
		ch := c.pending[msg.Id]
		delete(c.pending, msg.Id)
		c.mu.Unlock()
		if ch != nil {
			ch <- reply
		}
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead == nil {
		c.dead = err
	}
	for id, ch := range c.pending {
		ch <- spawnReply{err: err}
		delete(c.pending, id)
	}
}

// Run asks the helper to execute req. The helper captures the child's
// stdout/stderr and returns them inline in the ActionResult's Stdout/StderrRaw.
func (c *Client) Run(ctx context.Context, req *epb.SpawnRequest) (*epb.SpawnResult, error) {
	ch := make(chan spawnReply, 1)
	c.mu.Lock()
	if c.dead != nil {
		c.mu.Unlock()
		return nil, c.dead
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	start := &epb.SpawnMessage{Id: id, Payload: &epb.SpawnMessage_Start{Start: req}}
	if err := c.conn.send(start); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		// The helper owns and reaps the child, so let it cancel and report back;
		// signalling here would risk a pid-reuse race.
		cancel := &epb.SpawnMessage{Id: id, Payload: &epb.SpawnMessage_Cancel{Cancel: &epb.SpawnCancel{}}}
		if err := c.conn.send(cancel); err != nil {
			// Socket is dead; readLoop's failAll will also fail this Run. A late
			// failAll send to the cap-1 ch after we return is harmlessly discarded.
			c.mu.Lock()
			delete(c.pending, id)
			c.mu.Unlock()
			return nil, fmt.Errorf("send cancel after ctx done: %w", err)
		}
		reply := <-ch
		if reply.err != nil {
			// The helper's error crossed the wire as a string, losing the
			// context.Canceled identity; return the real cause so errors.Is
			// matches like on the in-process path. A success that raced the
			// cancel is still honored.
			return nil, context.Cause(ctx)
		}
		return reply.result, nil
	}
}

// Close closes the client end of the control socket, so the helper sees EOF and exits.
func (c *Client) Close() error {
	return c.conn.close()
}

// Kill sends SIGKILL to the helper process.
func (c *Client) Kill() error {
	return c.cmd.Process.Kill()
}

// Wait blocks until the helper process exits and reaps it.
func (c *Client) Wait() error {
	return c.cmd.Wait()
}
