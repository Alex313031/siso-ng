// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/google/subcommands"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
)

func (*diffCommand) Name() string {
	return "diff"
}

func (*diffCommand) Synopsis() string {
	return "diff siso hashfs data"
}

func (*diffCommand) Usage() string {
	return `show difference between two siso hashfs data.

 $ siso fs diff -C <dir>

It will print mismatched file entries between .siso_fs_state (--fs_state)
and .siso_fs_state.0 (--fs_state_base).
`
}

type diffCommand struct {
	dir           string
	stateFile     string
	stateFileBase string
	// TODO: options to compare mtime
}

func (c *diffCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory")
	flagSet.StringVar(&c.stateFile, "fs_state", stateFile, "fs_state filename")
	flagSet.StringVar(&c.stateFileBase, "fs_state_base", stateFile+".0", "fs_state filename for diff base")
}

func (c *diffCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := os.Chdir(c.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir %s: %v\n", c.dir, err)
		return 1
	}

	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: c.stateFile})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load %s: %v\n", c.stateFile, err)
		return 1
	}
	// sort files in build order.
	sort.Slice(st.Entries, func(i, j int) bool {
		if st.Entries[i].UpdatedTime != st.Entries[j].UpdatedTime {
			return st.Entries[i].UpdatedTime < st.Entries[j].UpdatedTime
		}
		return st.Entries[i].Id.GetModTime() < st.Entries[j].Id.GetModTime()
	})
	stm := hashfs.StateMap(st)
	stBase, err := hashfs.Load(ctx, hashfs.Option{StateFile: c.stateFileBase})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load %s: %v\n", c.stateFileBase, err)
		return 1
	}
	stBaseM := hashfs.StateMap(stBase)

	for _, s := range st.Entries {
		cur := stm[s.Name]
		base := stBaseM[s.Name]

		diff, found := checkDiff(s.Name, cur, base)
		if !found {
			continue
		}
		buf, err := json.MarshalIndent(diff, "", " ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal error: %v\n", err)
			return 1
		}
		fmt.Printf("%s\n", buf)
	}
	return 0
}

type entryDiff struct {
	Name     string    `json:"name"`
	DiffType string    `json:"diff_type"`
	Cur      *pb.Entry `json:"cur"`
	Base     *pb.Entry `json:"base,omitempty"`
}

func checkDiff(name string, cur, base *pb.Entry) (entryDiff, bool) {
	var diffType string
	switch {
	case proto.Equal(cur, base):
		return entryDiff{}, false
		// cur should not be nil.
	case base == nil:
		diffType = "new"
	case !proto.Equal(cur.Digest, base.Digest):
		diffType = "content_modified"
	case cur.IsExecutable != base.IsExecutable:
		diffType = "mode_modified"
	case cur.Target != base.Target:
		diffType = "symlink_modified"

		// followings will be restattable?
	case !bytes.Equal(cur.CmdHash, base.CmdHash):
		diffType = "cmdline_modified"
	case !proto.Equal(cur.Action, base.Action):
		diffType = "action_modified"
	case cur.Id.GetModTime() != base.Id.GetModTime():
		diffType = "mtime_modified"
	case cur.UpdatedTime != base.UpdatedTime:
		diffType = "clean_updated"
	default:
		diffType = "unknown"
	}
	return entryDiff{
		Name:     name,
		DiffType: diffType,
		Cur:      cur,
		Base:     base,
	}, true
}
