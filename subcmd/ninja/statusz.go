// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/o11y/clog"
)

func newStatuszServer(ctx context.Context, b *build.Builder, dir string) error {
	mux := http.NewServeMux()

	mux.Handle("/api/active_steps", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		activeSteps := b.ActiveSteps()
		buf, err := json.Marshal(activeSteps)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to json marshal: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Add("Context-Type", "text/json")
		_, err = w.Write(buf)
		if err != nil {
			clog.Warningf(ctx, "failed to write response: %v", err)
		}
	}))
	s := &http.Server{
		Handler: mux,
	}
	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", "localhost:0")
	if err != nil {
		clog.Warningf(ctx, "listener error: %v", err)
		return err
	}
	defer func() {
		err := listener.Close()
		if err != nil {
			clog.Warningf(ctx, "listener close error: %v", err)
		}
	}()

	s.Addr = listener.Addr().String()
	portFilename := filepath.Join(dir, ".siso_port")
	clog.Infof(ctx, "%s=%s", portFilename, s.Addr)
	err = os.WriteFile(portFilename, []byte(s.Addr), 0644)
	if err != nil {
		clog.Warningf(ctx, "failed to write %s: %v", portFilename, err)
	}
	defer func() {
		err := os.Remove(portFilename)
		if err != nil {
			clog.Warningf(ctx, "failed to remove %s: %v", portFilename, err)
		}
	}()

	go func() {
		<-ctx.Done()
		err := s.Close()
		if err != nil {
			clog.Warningf(ctx, "http close error: %v", err)
		}
	}()

	err = s.Serve(listener)
	if err != nil {
		clog.Warningf(ctx, "http serve error: %v", err)
	}
	return nil
}
