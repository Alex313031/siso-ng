// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapitest

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/test/bufconn"

	"go.chromium.org/build/kajiya/actioncache"
	"go.chromium.org/build/kajiya/blobstore"
	"go.chromium.org/build/kajiya/capabilities"
	"go.chromium.org/build/kajiya/execution"
	"go.chromium.org/build/kajiya/execution/localexec"
	"go.chromium.org/build/kajiya/server"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/reapi"
)

// NewLocalExec starts an in-process kajiya server backed by an nsjail-sandboxed
// localexec.Executor and returns a reapi client connected to it. The calling
// test is skipped where nsjail can't run (see skipUnlessNsjailUsable).
func NewLocalExec(ctx context.Context, t *testing.T) *reapi.Client {
	t.Helper()
	skipUnlessNsjailUsable(t)

	// 4 MiB matches kajiya's own execution-path bufconn tests.
	const bufSize = 4 * 1024 * 1024
	lis := bufconn.Listen(bufSize)
	t.Log("kajiya local-exec reapi over bufconn")

	dir := t.TempDir()
	serv := grpc.NewServer()
	cfg := server.Config{}
	capabilities.Register(serv, cfg)

	cas, err := blobstore.New(ctx, filepath.Join(dir, "cas"))
	if err != nil {
		lis.Close()
		t.Fatal(err)
	}
	blobstore.Register(serv, cas, cfg)

	ac, err := actioncache.New(ctx, filepath.Join(dir, "ac"), cas)
	if err != nil {
		lis.Close()
		t.Fatal(err)
	}
	if err := actioncache.Register(serv, ac, cas, cfg); err != nil {
		lis.Close()
		t.Fatal(err)
	}

	// allowHostFS=true so image-less actions can use host tools (python3).
	executor, err := localexec.New(filepath.Join(dir, "exec"), cas, localexec.OverlayFS, true, false)
	if err != nil {
		lis.Close()
		t.Fatal(err)
	}
	if err := execution.Register(serv, executor, ac, cas, cfg); err != nil {
		lis.Close()
		t.Fatal(err)
	}
	reflection.Register(serv)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		err := serv.Serve(lis)
		t.Logf("kajiya local-exec serve finished: %v", err)
	}()
	t.Cleanup(func() {
		serv.GracefulStop()
		lis.Close()
		<-closed
	})

	// passthrough:// avoids DNS-resolving the dummy target; the dialer returns the pipe.
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	opt := reapi.Option{
		Address:  "bufconn",
		Instance: "projects/siso-test/instances/default_instance",
	}
	client, err := reapi.NewFromConn(ctx, opt, cred.Cred{}, conn, conn, conn)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// skipUnlessNsjailUsable skips the test unless nsjail can actually run here:
// it may be installed yet unable to build its mount namespace in restricted
// environments (unprivileged CI containers), so probe with a trivial action.
func skipUnlessNsjailUsable(t *testing.T) {
	t.Helper()
	nsjailPath, err := exec.LookPath("nsjail")
	if err != nil {
		t.Skip("kajiya local-exec RBE requires nsjail: not found in PATH")
	}
	out, err := exec.Command(nsjailPath, "--quiet", "--chroot", "/", "--cwd", "/", "--disable_rlimits", "--", "/bin/true").CombinedOutput()
	if err != nil {
		t.Skipf("kajiya local-exec RBE requires a working nsjail: %v: %s", err, strings.TrimSpace(string(out)))
	}
}
