// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/admin"

	"go.chromium.org/build/siso/o11y/clog"
)

func NewStatuszServer(ctx context.Context, b *Builder, dir string) error {
	grpcServer := grpc.NewServer()
	adminCleanup, err := admin.Register(grpcServer)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()

	mux.Handle("/api/active_steps", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		activeSteps := b.ActiveSteps()
		buf, err := json.Marshal(activeSteps)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to json marshal: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(buf)
		if err != nil {
			clog.Warningf(ctx, "failed to write response: %v", err)
		}
	}))

	mux.Handle("/api/progress", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := b.ProgressInfo()
		buf, err := json.Marshal(p)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to json marshal: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(buf)
		if err != nil {
			clog.Warningf(ctx, "failed to write response: %v", err)
		}
	}))

	mainHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			mux.ServeHTTP(w, r)
		}
	})

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	s := &http.Server{
		Handler:   mainHandler,
		Protocols: &protocols,
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
		adminCleanup()
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
