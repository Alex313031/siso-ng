// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package isolate uploads and computes tree digest for each targets.
package isolate

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	log "github.com/golang/glog"
	"github.com/google/subcommands"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/grpclog"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/signals"
	"go.chromium.org/build/siso/ui"
)

const usage = `isolate uploads and computes tree digest for each targets.

 $ siso isolate -project <project> -reapi_instance <instance> \
    -C <dir> \
    -cas_instance projects/<cas project>/instances/<instance> \
    -dump_json <output json path> \
    <target> ...

`

// Cmd returns the Command for the `isolate` subcommand.
func Cmd(authOpts cred.Options) *Command {
	return &Command{
		authOpts: authOpts,
	}
}

func (*Command) Name() string {
	return "isolate"
}

func (*Command) Synopsis() string {
	return "isolate uploads and computes tree digests"
}

func (*Command) Usage() string {
	return usage
}

// Command implements isolate subcommand.
type Command struct {
	Flags     *flag.FlagSet
	authOpts  cred.Options
	projectID string
	reopt     *reapi.Option
	casopt    *reapi.Option

	dir string

	fsopt *hashfs.Option

	dumpJSON string

	jobID              string
	enableCloudLogging bool
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can be set by $SISO_PROJECT")
	c.reopt = new(reapi.Option)
	c.reopt.RegisterFlags(flagSet, reapi.Envs("REAPI"))
	c.casopt = new(reapi.Option)
	c.casopt.Prefix = "cas"
	c.casopt.RegisterFlags(flagSet, reapi.Envs("DEST_CASS"))

	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory")

	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(flagSet)

	flagSet.StringVar(&c.dumpJSON, "dump_json", "", "dump in json file")

	flagSet.StringVar(&c.jobID, "job_id", uuid.New().String(), "ID for a grouping of related builds such as a Buildbucket job. ")
	flagSet.BoolVar(&c.enableCloudLogging, "enable_cloud_logging", true, "enable cloud logging")
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	c.Flags = flagSet
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%s\n", usage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

type errInterrupted struct{}

func (errInterrupted) Error() string        { return "interrupt by signal" }
func (errInterrupted) Is(target error) bool { return target == context.Canceled }

func (c *Command) run(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer signals.HandleInterrupt(ctx, func() {
		cancel(errInterrupted{})
	})()
	started := time.Now()
	execRoot, err := c.initWorkdirs(ctx)
	if err != nil {
		return err
	}
	if len(c.jobID) > 1024 {
		return fmt.Errorf("-job_id length %d must be less than 1024", len(c.jobID))
	}
	projectID := c.reopt.UpdateProjectID(c.projectID)
	err = c.reopt.CheckValid()
	if err != nil {
		return fmt.Errorf("reapi option is invalid: %w", err)
	}
	spin := ui.Default.NewSpinner()
	var credential cred.Cred
	if c.reopt.NeedCred() || c.enableCloudLogging {
		spin.Start("init credentials")
		credential, err = cred.New(ctx, c.reopt.ServiceURI(), c.authOpts)
		if err != nil {
			spin.Stop(errors.New(""))
			return err
		}
		spin.Stop(nil)
	}
	if c.enableCloudLogging {
		logCtx, loggerURL, done, err := c.initCloudLogging(ctx, projectID, execRoot, credential)
		if err != nil {
			// b/335295396 Compile step hitting write requests quota
			// rather than build fails, fallback to glog.
			fmt.Fprintf(os.Stderr, "cloud logging: %v\n", err)
			fmt.Fprintln(os.Stderr, "fallback to glog")
			c.enableCloudLogging = false
		} else {
			fmt.Fprintln(os.Stderr, loggerURL)
			defer done()
			ctx = logCtx
		}
	}

	ui.Default.Infof(fmt.Sprintf("use %s\n", c.reopt))
	client, err := reapi.New(ctx, credential, *c.reopt)
	if err != nil {
		return fmt.Errorf("failed to initialize reapi client: %w", err)
	}
	defer func() {
		err := client.Close()
		if err != nil {
			clog.Errorf(ctx, "close reapi client: %v", err)
		}
	}()
	artifactStore := client.CacheStore()

	ui.Default.PrintLines(fmt.Sprintf("target cas instance: %s\n", c.casopt.Instance))

	ccred, err := c.casCred(ctx)
	if err != nil {
		return fmt.Errorf("failed to get cas credential: %w", err)
	}
	casClient, err := reapi.New(ctx, ccred, *c.casopt)
	if err != nil {
		return fmt.Errorf("failed to initialize cas client: %w", err)
	}
	defer func() {
		err := casClient.Close()
		if err != nil {
			clog.Errorf(ctx, "close cas client: %v", err)
		}
	}()

	st, err := hashfs.Load(ctx, *c.fsopt)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", c.fsopt.StateFile, err)
	}
	c.fsopt.StateFile = ""
	c.fsopt.DataSource = artifactStore
	c.fsopt.OutputLocal = func(ctx context.Context, fname string) bool {
		return false
	}
	hashFS, err := hashfs.New(ctx, *c.fsopt)
	if err != nil {
		return err
	}
	err = hashFS.SetState(ctx, st)
	if err != nil {
		return err
	}
	err = hashFS.WaitReady(ctx)
	if err != nil {
		return err
	}
	var (
		mu     sync.Mutex
		result = make(map[string]string)
	)
	eg, ectx := errgroup.WithContext(ctx)
	for _, target := range c.Flags.Args() {
		eg.Go(func() error {
			targetStarted := time.Now()
			d, err := upload(ectx, execRoot, c.dir, hashFS, casClient, target)
			duration := time.Since(targetStarted)
			if err != nil {
				return fmt.Errorf("failed for %s in %s: %w", target, duration, err)
			}
			mu.Lock()
			result[target] = d.String()
			mu.Unlock()
			clog.Infof(ectx, "uploaded digest for %s: %s in %s", target, d, duration)
			ui.Default.PrintLines(fmt.Sprintf("uploaded digest for %s: %s in %s\n", target, d, duration))
			return nil
		})
	}
	err = eg.Wait()
	if err != nil {
		return err
	}
	if c.dumpJSON != "" {
		buf, err := json.MarshalIndent(result, "", " ")
		if err != nil {
			return err
		}
		err = os.WriteFile(c.dumpJSON, buf, 0644)
		if err != nil {
			return err
		}
	}
	ui.Default.PrintLines(fmt.Sprintf("done %s\n", time.Since(started)))
	return nil
}

func (c *Command) initWorkdirs(ctx context.Context) (string, error) {
	// don't use $PWD for current directory
	// to avoid symlink issue. b/286779149
	pwd := os.Getenv("PWD")
	_ = os.Unsetenv("PWD") // no error for safe env key name.

	execRoot, err := os.Getwd()
	if pwd != "" {
		_ = os.Setenv("PWD", pwd) // no error to reset env with valid value.
	}
	if err != nil {
		return "", err
	}
	clog.Infof(ctx, "wd: %s", execRoot)
	err = os.Chdir(c.dir)
	if err != nil {
		return "", err
	}
	clog.Infof(ctx, "change dir to %s", c.dir)
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	clog.Infof(ctx, "exec_root: %s", execRoot)

	// recalculate dir as relative to exec_root.
	// recipe may use absolute path for -C.
	rdir, err := filepath.Rel(execRoot, cwd)
	if err != nil {
		return "", err
	}
	if !filepath.IsLocal(rdir) {
		return "", fmt.Errorf("dir %q is out of exec root %q", cwd, execRoot)
	}
	c.dir = rdir
	clog.Infof(ctx, "working_directory in exec_root: %s", c.dir)
	return execRoot, err
}

func (c *Command) casCred(ctx context.Context) (cred.Cred, error) {
	if c.casopt.Instance == "default_instance" || c.casopt.Instance == "" {
		return cred.Cred{}, fmt.Errorf("-cas_instance must be set")
	}
	if !strings.HasPrefix(c.casopt.Instance, "projects/") {
		return cred.Cred{}, fmt.Errorf(
			"-cas_instance must be in projects/<project>/instances/<instance> format. got %q", c.casopt.Instance)
	}
	project := strings.Split(c.casopt.Instance, "/")[1]
	// Use Swarming specific authentication mechanism.
	authOpts := cred.AuthOpts("luci-auth",
		"context",
		"--act-as-service-account",
		fmt.Sprintf("cas-read-write@%s.iam.gserviceaccount.com", project),
		"--act-via-realm",
		fmt.Sprintf("@internal:%s/cas-read-write", project),
	)
	return cred.New(ctx, c.casopt.ServiceURI(), authOpts)
}

func upload(ctx context.Context, execRoot, buildDir string, hashFS *hashfs.HashFS, casClient *reapi.Client, target string) (digest.Digest, error) {
	isolateName := fmt.Sprintf("%s.isolate", target)
	buf, err := os.ReadFile(isolateName)
	if err != nil {
		return digest.Digest{}, err
	}
	v := make(map[string]any)
	err = json.Unmarshal(buf, &v)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to unmarshal %s: %w", isolateName, err)
	}
	variables, ok := v["variables"].(map[string]any)
	if !ok {
		return digest.Digest{}, fmt.Errorf(`no "variables" in %s`, isolateName)
	}
	filesArray, ok := variables["files"].([]any)
	if !ok {
		return digest.Digest{}, fmt.Errorf(`no "variables.files" in %s`, isolateName)
	}
	// Construct a CAS tree, traversing directories after expanding directory entries.
	// Some files are ignored by isolate command by default. e.g. *.pyc, .git/ dir.
	// See also https://crrev.com/9ec59f1bc4603981e8ebb9c8fccfd16a311fd7fa/client/isolate/isolate.go#93
	fnames := make([]string, 0, len(filesArray))
	for i, f := range filesArray {
		fname, ok := f.(string)
		if !ok {
			return digest.Digest{}, fmt.Errorf(`not string in "variables.files[%d]" %v (%T)`, i, f, f)
		}
		// Expand directory entries.
		pathname := filepath.ToSlash(filepath.Join(buildDir, fname))
		fi, err := hashFS.Stat(ctx, execRoot, pathname)
		if err != nil {
			return digest.Digest{}, err
		}
		if !fi.IsDir() {
			fnames = append(fnames, fname)
			continue
		}
		clog.Infof(ctx, "expand dir %s", pathname)
		fsys := hashFS.FileSystem(ctx, filepath.Join(execRoot, pathname))
		err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == ".git" {
				return fs.SkipDir
			}
			fnames = append(fnames, filepath.ToSlash(filepath.Join(fname, path)))
			return nil
		})
		if err != nil {
			return digest.Digest{}, err
		}
	}
	ds := digest.NewStore()
	tree := merkletree.New(ds)
	for _, fname := range fnames {
		pathname := filepath.ToSlash(filepath.Join(buildDir, fname))
		// To match with the implementation of `isolate` command,
		// exclude only *.pyc file, while keeping an empty __pycache__/ dir.
		if strings.HasSuffix(pathname, ".pyc") {
			continue
		}
		ents, err := hashFS.Entries(ctx, execRoot, []string{pathname})
		if err != nil {
			return digest.Digest{}, err
		}
		if len(ents) == 0 {
			return digest.Digest{}, fmt.Errorf("no digest for %s", pathname)
		}
		ent := ents[0]
		// To match with the implementation of `isolate` command,
		// trim '/' suffix from symlink targets.
		// See also https://github.com/bazelbuild/remote-apis-sdks/blob/f4821a2a072c44f9af83002cf7a272fff8223fa3/go/pkg/cas/upload.go#L790C11-L790C25
		if ent.Target != "" {
			ent.Target = filepath.Clean(ent.Target)
		}
		err = tree.Set(ent)
		if err != nil {
			return digest.Digest{}, err
		}
	}
	d, err := tree.Build(ctx)
	if err != nil {
		return digest.Digest{}, err
	}
	clog.Infof(ctx, "upload %s for %s", d, target)
	started := time.Now()
	n, err := casClient.UploadAll(ctx, ds)
	if err != nil {
		return digest.Digest{}, err
	}
	clog.Infof(ctx, "uploaded %d for %s in %s", n, target, time.Since(started))
	return d, nil
}

func (c *Command) initCloudLogging(ctx context.Context, projectID, execRoot string, credential cred.Cred) (context.Context, string, func(), error) {
	taskID := uuid.New().String()
	log.Infof("enable cloud logging project=%s id=%s", projectID, taskID)

	// log_id: "siso.log" and "siso.step"
	// use generic_task resource
	// https://cloud.google.com/logging/docs/api/v2/resource-list
	// https://cloud.google.com/monitoring/api/resources#tag_generic_task
	client, err := logging.NewClient(ctx, projectID, credential.ClientOptions()...)
	if err != nil {
		return ctx, "", func() {}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return ctx, "", func() {}, err
	}
	logger, err := clog.New(ctx, client, "siso.log", "siso.step", &mrpb.MonitoredResource{
		Type: "generic_task",
		// should set labels for generic_task.
		// see https://cloud.google.com/logging/docs/api/v2/resource-list
		Labels: map[string]string{
			"project_id": projectID,
			"job":        c.jobID,
			"task_id":    taskID,
			"location":   hostname,
			"namespace":  execRoot,
		},
	})
	if err != nil {
		return ctx, "", func() {}, err
	}
	ctx = clog.NewContext(ctx, logger)
	grpclog.SetLoggerV2(logger)
	return ctx, logger.URL(), func() {
		errch := make(chan error, 1)
		go func() {
			errch <- logger.Close()
		}()
		timeout := 10 * time.Second
		// Don't use clog as it's closing Cloud logging client.
		select {
		case <-time.After(timeout):
			log.Warningf("close not finished in %s", timeout)
		case err := <-errch:
			if err != nil {
				log.Warningf("falied to close Cloud logger: %v", err)
			}
		}
	}, nil
}
