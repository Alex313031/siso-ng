// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"time"

	log "github.com/golang/glog"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/retry"
)

// WalkDir walks the directory tree rooted identified by d,
// calling fn for each directory in the tree, including root.
func (c *Client) WalkDir(ctx context.Context, d digest.Digest, fn func(dname string, dir *rpb.Directory) error) error {
	if c == nil {
		return fmt.Errorf("reapi is not configured")
	}
	ctx, span := trace.NewSpan(ctx, "reapi-walkdir")
	defer span.Close(nil)

	casClient := rpb.NewContentAddressableStorageClient(c.casConn)
	var pageToken string
	dirs := make(map[string]digest.Digest)      // dname -> digest
	waiting := make(map[digest.Digest][]string) // digest -> dnames
	waiting[d] = append(waiting[d], "")
	known := make(map[digest.Digest]*rpb.Directory)
	var apiDur, cbDur time.Duration
	var nrecvs, ndirs int
	err := retry.Do(ctx, func() error {
		stream, err := casClient.GetTree(ctx, &rpb.GetTreeRequest{
			InstanceName: c.opt.Instance,
			RootDigest:   d.Proto(),
			PageToken:    pageToken,
		})
		if err != nil {
			return err
		}
		for {
			started := time.Now()
			resp, err := stream.Recv()
			dur := time.Since(started)
			apiDur += dur
			nrecvs++
			if errors.Is(err, io.EOF) {
				// all directories received.
				return nil
			}
			if err != nil {
				return err
			}
			if log.V(1) {
				clog.Infof(ctx, "recv %d %s", len(resp.Directories), dur)
			}
			var handleDir func(d digest.Digest, dir *rpb.Directory) error
			handleDir = func(d digest.Digest, dir *rpb.Directory) error {
				if len(waiting[d]) == 0 {
					return nil
				}
				var dnames []string
				dnames, waiting[d] = waiting[d], nil
				for _, dname := range dnames {
					dirs[dname] = d
					started = time.Now()
					err = fn(dname, dir)
					cbDur += time.Since(started)
					ndirs++
					if err != nil {
						return err
					}
					for _, subdir := range dir.Directories {
						sd := digest.FromProto(subdir.Digest)
						subname := path.Join(dname, subdir.Name)
						waiting[sd] = append(waiting[sd], subname)
						if sdir := known[sd]; sdir != nil {
							// check loop
							for pname := dname; pname != "" && pname != "."; pname = path.Dir(pname) {
								if dirs[pname] == sd {
									return fmt.Errorf("directory loop %s from %s -> %s", sd, subname, pname)
								}
							}
							err := handleDir(sd, sdir)
							if err != nil {
								return err
							}
							return nil
						}
					}
				}
				return nil
			}
			for _, dir := range resp.Directories {
				dd, derr := digest.FromProtoMessage(dir)
				if derr != nil {
					return derr
				}
				if known[dd.Digest()] != nil {
					clog.Warningf(ctx, "duplicate dir %s", dd.Digest())
					continue
				}
				known[dd.Digest()] = dir
				err := handleDir(dd.Digest(), dir)
				if err != nil {
					return err
				}
			}
			pageToken = resp.NextPageToken
		}
	})
	var avgAPIDur time.Duration
	if nrecvs > 0 {
		avgAPIDur = apiDur / time.Duration(nrecvs)
	}
	var avgCBDur time.Duration
	if ndirs > 0 {
		avgCBDur = cbDur / time.Duration(ndirs)
	}
	clog.Infof(ctx, "walkdir %s api %d %s avg:%s dir %d cb %s avg:%s",
		d, nrecvs, apiDur, avgAPIDur, ndirs, cbDur, avgCBDur)
	return err
}
