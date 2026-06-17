// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"bufio"
	"net"
	"sync"

	"google.golang.org/protobuf/encoding/protodelim"

	epb "go.chromium.org/build/siso/execute/proto"
)

// spawnUnmarshalOpts disables protodelim's default 4 MiB message cap: we control
// both ends of this socketpair, so a frame is never corrupt, and a result can
// legitimately exceed 4 MiB (it carries the child's captured output).
var spawnUnmarshalOpts = protodelim.UnmarshalOptions{MaxSize: -1}

// spawnConn is a framed transport over a unix socket. recv is called by one
// goroutine; send may be concurrent and serializes writes.
type spawnConn struct {
	c   *net.UnixConn
	r   *bufio.Reader
	wmu sync.Mutex
}

func newSpawnConn(c *net.UnixConn) *spawnConn {
	return &spawnConn{
		c: c,
		r: bufio.NewReader(c),
	}
}

func (s *spawnConn) close() error {
	return s.c.Close()
}

// send writes one length-delimited SpawnMessage. Concurrent sends serialize on
// wmu so messages never interleave on the stream.
func (s *spawnConn) send(msg *epb.SpawnMessage) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := protodelim.MarshalTo(s.c, msg)
	return err
}

// recv reads one length-delimited SpawnMessage.
func (s *spawnConn) recv() (*epb.SpawnMessage, error) {
	msg := &epb.SpawnMessage{}
	if err := spawnUnmarshalOpts.UnmarshalFrom(s.r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
