// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package metadata provides a data structure to hold build metadata.
package metadata

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"

	"go.chromium.org/build/siso/runtimex"
)

// Metadata contains structured metadata for the build.
// It can hold arbitrary key-value pairs, but some keys are well-known:
//   - num_cpu: number of CPUs
//   - goos: the value of Go's GOOS constant
//   - goarch: the value of Go's GOARCH constant
type Metadata struct {
	entries map[string]string
}

// New returns an initialized Metadata struct.
func New() Metadata {
	metadata := Metadata{
		entries: make(map[string]string),
	}
	// Set the well-known keys that are not set by calls to Set.
	metadata.entries["num_cpu"] = strconv.Itoa(runtimex.NumCPU())
	metadata.entries["goos"] = runtime.GOOS
	metadata.entries["goarch"] = runtime.GOARCH
	// TODO: want to include hostname, too?
	return metadata
}

// Keys returns a list of all available keys in the metadata.
func (md Metadata) Keys() []string {
	keys := make([]string, 0, len(md.entries))
	for k := range md.entries {
		keys = append(keys, k)
	}
	return keys
}

// SortedKeys returns a sorted list of all available keys in the metadata.
func (md Metadata) SortedKeys() []string {
	keys := md.Keys()
	sort.Strings(keys)
	return keys
}

// Set sets a key-value pair in the metadata.
func (md Metadata) Set(key, value string) error {
	// Return an error if the caller tries to set a well-known key.
	switch key {
	case "num_cpu", "goos", "goarch":
		return fmt.Errorf("cannot override well-known key %q in metadata", key)
	}
	md.entries[key] = value
	return nil
}

// Get returns the value for the given key. If the key is not set, it returns
// the empty string.
func (md Metadata) Get(key string) string {
	return md.entries[key]
}

// Size returns the number of key-value pairs in the metadata.
func (md Metadata) Size() int {
	return len(md.entries)
}
