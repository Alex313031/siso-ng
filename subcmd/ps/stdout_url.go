// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ps

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/ui"
)

type stdoutURLSource struct {
	stdoutURL string

	steps   sync.Map
	mu      sync.Mutex
	lines   []string
	started time.Time
	done    chan bool
}

func newStdoutURLSource(ctx context.Context, stdoutURL string) (*stdoutURLSource, error) {
	var authorization string
	if strings.HasPrefix(stdoutURL, "https://logs.chromium.org/logs/") {
		if !strings.HasSuffix(stdoutURL, "?format=raw") {
			stdoutURL += "?format=raw"
		}
		cmd := exec.Command("luci-auth", "token")
		out, err := cmd.Output()
		if err != nil {
			clog.Warningf(ctx, "failed to get credential by 'luch-auth token': %v", err)
		} else {
			authorization = "Bearer " + strings.TrimSpace(string(out))
		}
	}
	clog.Infof(ctx, "check %s", stdoutURL)
	req, err := http.NewRequestWithContext(ctx, "GET", stdoutURL, nil)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		req.Header.Add("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", stdoutURL, err)
	}
	src := &stdoutURLSource{
		stdoutURL: stdoutURL,
		started:   time.Now(),
		done:      make(chan bool),
	}
	go src.run(ctx, resp.Body)
	return src, nil
}

func (s *stdoutURLSource) location() string {
	return s.stdoutURL
}

func (s *stdoutURLSource) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

func (s *stdoutURLSource) run(ctx context.Context, body io.ReadCloser) {
	defer func() {
		err := body.Close()
		if err != nil {
			clog.Warningf(ctx, "close %v", err)
		}
	}()
	rd := bufio.NewReader(body)
	for {
		buf, err := rd.ReadBytes('\n')
		if err != nil {
			clog.Errorf(ctx, "read %v", err)
			close(s.done)
			return
		}
		line := string(buf[:len(buf)-1])
		if !strings.HasPrefix(line, "[") {
			s.mu.Lock()
			s.lines = append(s.lines, line)
			s.mu.Unlock()
			continue
		}
		fields := strings.SplitN(line, " ", 4)
		dur, err := time.ParseDuration(fields[1])
		if err != nil {
			clog.Warningf(ctx, "%s: dur=%q: %v", fields[3], fields[1], err)
		}
		s.mu.Lock()
		started := time.Now().Add(-dur)
		if started.Before(s.started) {
			s.started = started
		}
		s.mu.Unlock()
		switch fields[2] {
		case "S":
			s.steps.Store(fields[3], dur)
		case "F", "c":
			s.steps.Delete(fields[3])
		default:
			clog.Warningf(ctx, "%s: unknown state=%s", fields[3], fields[2])
		}
	}
}

func (s *stdoutURLSource) fetch(ctx context.Context) ([]build.ActiveStepInfo, error) {
	actives := map[string]time.Duration{}
	s.steps.Range(func(key, value any) bool {
		name := key.(string)
		dur := value.(time.Duration)
		actives[name] = dur
		return true
	})
	var names []string
	for name := range actives {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if actives[names[i]] != actives[names[j]] {
			return actives[names[i]] < actives[names[j]]
		}
		return names[i] < names[j]
	})
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	dur := time.Since(started)
	var activeSteps []build.ActiveStepInfo
	for _, name := range names {
		activeSteps = append(activeSteps, build.ActiveStepInfo{
			Desc: name,
			Dur:  ui.FormatDuration(dur - actives[name]),
		})
	}
	var err error
	select {
	case <-ctx.Done():
		err = context.Cause(ctx)
	case <-s.done:
		err = io.EOF
	default:
	}
	return activeSteps, err
}
