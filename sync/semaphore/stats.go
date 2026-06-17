// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package semaphore

import (
	"sync"
	"time"
)

type Stat struct {
	// Name is semaphore name.
	Name string

	// N is count that the semaphore was used.
	N int

	// NErr is count that error reported when semaphore was used.
	NErr int

	// Total duration for wait and serv for semaphore.
	waitTotal, servTotal time.Duration

	// Max duration for wait and serv for semaphore.
	WaitMax, ServMax time.Duration

	// Buckets by durations.
	//
	//  0: [0,10ms)
	//  1: [10ms, 100ms)
	//  2: [100ms, 1s)
	//  3: [1s, 10s)
	//  4: [10s, 1m)
	//  5: [1m, 10m)
	//  6: >=10m
	WaitBuckets, ServBuckets [7]int
}

func (s *Stat) WaitAvg() time.Duration {
	if s.N == 0 {
		return 0
	}
	return s.waitTotal / time.Duration(int64(s.N))
}

func (s *Stat) ServAvg() time.Duration {
	if s.N == 0 {
		return 0
	}
	return s.servTotal / time.Duration(int64(s.N))
}

func (s *Stat) Update(wait, serv time.Duration, isErr bool) {
	s.N++
	if isErr {
		s.NErr++
	}
	s.waitTotal += wait
	s.servTotal += serv
	if s.WaitMax < wait {
		s.WaitMax = wait
	}
	if s.ServMax < serv {
		s.ServMax = serv
	}
	s.WaitBuckets[bucketIndex(wait)]++
	s.ServBuckets[bucketIndex(serv)]++
}

func newStat(name string, wait, serv *stat) Stat {
	s := Stat{
		Name: name,
	}
	wait.mu.Lock()
	s.N = wait.n
	s.NErr = wait.nerr
	s.waitTotal = wait.total
	s.WaitMax = wait.max
	s.WaitBuckets = wait.buckets
	wait.mu.Unlock()

	serv.mu.Lock()
	s.N = max(s.N, serv.n)
	s.NErr = max(s.NErr, serv.nerr)
	s.servTotal = serv.total
	s.ServMax = serv.max
	s.ServBuckets = serv.buckets
	serv.mu.Unlock()
	return s
}

type stat struct {
	mu      sync.Mutex
	n       int
	nerr    int
	total   time.Duration
	max     time.Duration
	buckets [7]int
}

func bucketIndex(dur time.Duration) int {
	switch {
	case dur < 10*time.Millisecond:
		return 0
	case dur < 100*time.Millisecond:
		return 1
	case dur < 1*time.Second:
		return 2
	case dur < 10*time.Second:
		return 3
	case dur < 1*time.Minute:
		return 4
	case dur < 10*time.Minute:
		return 5
	default:
		return 6
	}
}

func (s *stat) update(dur time.Duration, isErr bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if isErr {
		s.nerr++
	}
	s.total += dur
	if s.max < dur {
		s.max = dur
	}
	s.buckets[bucketIndex(dur)]++
}
