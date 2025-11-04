// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File name of ninja log.
const ninjaLogName = ".ninja_log"

// Ninja log format version.
const ninjaLogVersion = 5

// InitializeNinjaLog creates or truncates the ninja log file (.ninja_log) for writing
// and writes the version header.
func InitializeNinjaLog(builddir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(builddir, ninjaLogName), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "# ninja log v%d\n", ninjaLogVersion)
	return f, nil
}

// WriteNinjaLogEntries writes ninja log entries for a command.
// Note that the log entries are not compatible with Ninja, yet.
// TODO: b/298594790
//   - Implement MurmurHash64A as Ninja.
//   - Make mtime compatible on Windows.
func WriteNinjaLogEntries(ctx context.Context, w io.Writer, start, end int64, mtime time.Time, outputs, command []string) {
	h := fnv.New64()
	h.Write([]byte(strings.Join(command, " ")))
	hash := hex.EncodeToString(h.Sum(nil))
	for _, out := range outputs {
		fmt.Fprintf(w, "%d\t%d\t%d\t%s\t%s\n", start, end, mtime.UnixNano(), out, hash)
	}
}
