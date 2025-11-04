// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

type sseServer struct {
	// clients stores currently active clients.
	clients map[chan sseMessage]struct{}
	// newClients receives newly-connected clients and adds them to clients.
	newClients chan chan sseMessage
	// disconnectedClients receives disconnected clients and removes them from clients.
	disconnectedClients chan chan sseMessage
	// messages receives strings to send to all client channels.
	messages chan sseMessage
}

type sseMessage struct {
	event   string
	message string
}

func newSseServer() *sseServer {
	return &sseServer{
		clients:             make(map[chan sseMessage]struct{}),
		newClients:          make(chan chan sseMessage),
		disconnectedClients: make(chan chan sseMessage),
		messages:            make(chan sseMessage),
	}
}

// Start starts the HTTP SSE server loop.
func (s *sseServer) Start() {
	go func() {
		lastInfo := time.Now()
		sinceLastInfo := make(map[string]int)
		for {
			select {
			case c := <-s.newClients:
				s.clients[c] = struct{}{}
				fmt.Fprintf(os.Stderr, "SSE new client\n")
			case c := <-s.disconnectedClients:
				delete(s.clients, c)
				close(c)
				fmt.Fprintf(os.Stderr, "SSE removed client\n")
			case msg := <-s.messages:
				sinceLastInfo[msg.event]++
				for c := range s.clients {
					c <- msg
				}
			}
			if time.Since(lastInfo) > 5*time.Second {
				if len(sinceLastInfo) > 0 {
					fmt.Fprintf(os.Stderr, "SSE broadcasts: ")
					for event, count := range sinceLastInfo {
						fmt.Fprintf(os.Stderr, "%s %d, ", event, count)
					}
					fmt.Fprintf(os.Stderr, "cur %d clients\n", len(s.clients))
					clear(sinceLastInfo)
				}
				lastInfo = time.Now()
			}
		}
	}()
}

// ServeHTTP is an http.Handler that creates a channel to broadcast HTTP SSE messages until the HTTP client disconnects.
func (s *sseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Make sure that the writer supports flushing.
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	messageChan := make(chan sseMessage)
	s.newClients <- messageChan

	contextDone := r.Context().Done()
	go func() {
		<-contextDone
		s.disconnectedClients <- messageChan
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	for {
		msg, open := <-messageChan
		if !open {
			break
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.event, msg.message)
		f.Flush()
	}
}
