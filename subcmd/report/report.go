// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package report is report subcommand to report siso logs.
package report

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/hashfs/osfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/signals"
	"go.chromium.org/build/siso/ui"
)

const usage = `report siso logs
Collect siso logs in <dir>.

 $ siso report -C <dir>
`

// Cmd returns the Command for the `report` subcommand provided by this package.
func Cmd() *Command {
	return &Command{}
}

func (*Command) Name() string {
	return "report"
}

func (*Command) Synopsis() string {
	return "report siso logs"
}

func (*Command) Usage() string {
	return usage
}

// Command implements report subcommand.
type Command struct {
	dir     string
	osfsopt osfs.Option
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.osfsopt.RegisterFlags(flagSet)
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, usage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *Command) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(ctx, cancel)()

	clog.Infof(ctx, "dir %s", c.dir)
	err := os.Chdir(c.dir)
	if err != nil {
		return err
	}
	// TODO: upload report to make it easy to share.
	return c.archive(ctx)
}

func (c *Command) collect(ctx context.Context) (map[string]digest.Data, error) {
	report := make(map[string]digest.Data)
	fsys := os.DirFS(".")
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	osfs := osfs.New(ctx, "fs", c.osfsopt)

	for _, pat := range []string{"siso*", ".siso*", "args.gn", "gn_logs.txt"} {
		matches, err := fs.Glob(fsys, pat)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			continue
		}
		for _, fname := range matches {
			fi, err := os.Stat(fname)
			if errors.Is(err, fs.ErrNotExist) {
				// dangling symlink or so?
				continue
			}
			if fi.IsDir() {
				err = collectInDir(ctx, fsys, osfs, fname, report)
				if err != nil {
					clog.Errorf(ctx, "failed to collect in dir %s: %v", fname, err)
				}
				continue
			}
			ui.Default.PrintLines(fmt.Sprintf("reading %s", fname))
			localFname := fname
			if strings.HasSuffix(fname, ".redirected") {
				buf, err := os.ReadFile(fname)
				if err != nil {
					clog.Warningf(ctx, "failed to read %s: %v", fname, err)
					continue
				}
				localFname = string(buf)
				fname = strings.TrimSuffix(fname, ".redirected")
				clog.Infof(ctx, "%s -> %s", fname, localFname)
			}
			src := osfs.FileSource(localFname, -1)
			data, err := digest.FromLocalFile(ctx, src)
			if err != nil {
				clog.Errorf(ctx, "Error to calculate digest %s: %v", fname, err)
			} else {
				clog.Infof(ctx, "add %s %s", fname, data.Digest())
				report[fname] = data
			}
		}
	}
	if len(report) == 0 {
		return nil, fmt.Errorf("no siso files in %s: did you specify correct `-C <dir>` ?", wd)
	}

	// no need to collect .reproxy_tmp/racing
	// .reproxy_tmp/cache may exist, but must not collect reproxy.creds.
	_, err = os.Stat(".reproxy_tmp/logs")
	if err != nil {
		clog.Infof(ctx, "no .reproxy_tmp/logs: %v", err)
		return report, nil
	}
	err = collectInDir(ctx, fsys, osfs, ".reproxy_tmp/logs", report)
	if err != nil {
		clog.Errorf(ctx, "failed to collect in .reproxy_tmp/logs: %v", err)
	}
	return report, nil
}

func collectInDir(ctx context.Context, fsys fs.FS, osfs *osfs.OSFS, dname string, report map[string]digest.Data) error {
	return fs.WalkDir(fsys, dname, func(fname string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			clog.Infof(ctx, "skip dir %s", fname)
			return nil
		}
		ui.Default.PrintLines(fmt.Sprintf("reading %s", fname))
		src := osfs.FileSource(fname, -1)
		data, err := digest.FromLocalFile(ctx, src)
		if err != nil {
			clog.Errorf(ctx, "Error to calculate digest %s: %v", fname, err)
			return nil
		}
		clog.Infof(ctx, "add %s %s", fname, data.Digest())
		report[fname] = data
		return nil
	})
}

func (c *Command) archive(ctx context.Context) (err error) {
	report, err := c.collect(ctx)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "siso-report-*.tgz")
	if err != nil {
		return err
	}
	defer func() {
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
	}()
	gw := gzip.NewWriter(f)
	defer func() {
		cerr := gw.Close()
		if err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(gw)
	defer func() {
		cerr := tw.Close()
		if err == nil {
			err = cerr
		}
	}()

	var fnames []string
	for fname := range report {
		fnames = append(fnames, fname)
	}
	sort.Strings(fnames)
	now := time.Now()
	for _, fname := range fnames {
		ui.Default.PrintLines(fmt.Sprintf("packing %s", fname))
		buf, err := digest.DataToBytes(ctx, report[fname])
		if err != nil {
			return fmt.Errorf("failed to get bytes for %s: %w", fname, err)
		}
		err = tw.WriteHeader(&tar.Header{
			Name:    fname,
			Size:    int64(len(buf)),
			Mode:    0644,
			ModTime: now,
		})
		if err != nil {
			return fmt.Errorf("failed to write header for %s: %w", fname, err)
		}
		_, err = tw.Write(buf)
		if err != nil {
			return fmt.Errorf("failed to write data of %s: %w", fname, err)
		}
	}
	ui.Default.PrintLines(fmt.Sprintf("report file: %s\n\n", f.Name()))
	return tw.Flush()
}
