// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package execute runs commands.
package execute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	semverpb "github.com/bazelbuild/remote-apis/build/bazel/semver"
	log "github.com/golang/glog"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// Executor is an interface to run the cmd.
type Executor interface {
	Run(ctx context.Context, cmd *Cmd) error
}

// REProxyConfig specifies configuration options for using reproxy.
type REProxyConfig struct {
	CanonicalizeWorkingDir bool              `json:"canonicalize_working_dir,omitempty"`
	DownloadOutputs        bool              `json:"download_outputs,omitempty"`
	PreserveSymlinks       bool              `json:"preserve_symlinks,omitempty"`
	ExecStrategy           string            `json:"exec_strategy,omitempty"`
	ExecTimeout            string            `json:"exec_timeout,omitempty"`     // duration format
	ReclientTimeout        string            `json:"reclient_timeout,omitempty"` // duration format
	Inputs                 []string          `json:"inputs,omitempty"`
	ToolchainInputs        []string          `json:"toolchain_inputs,omitempty"`
	Labels                 map[string]string `json:"labels,omitempty"`
	Platform               map[string]string `json:"platform,omitempty"`
	RemoteWrapper          string            `json:"remote_wrapper,omitempty"`
	ServerAddress          string            `json:"server_address,omitempty"`
}

// Copy returns a deep copy of *REProxyConfig which can be safely mutated.
// Will return nil if given *REProxyConfig is nil.
func (c *REProxyConfig) Copy() *REProxyConfig {
	if c == nil {
		return nil
	}
	copy := *c
	copy.Labels = make(map[string]string, len(c.Labels))
	for k := range c.Labels {
		copy.Labels[k] = c.Labels[k]
	}
	copy.Platform = make(map[string]string, len(c.Platform))
	for k := range c.Platform {
		copy.Platform[k] = c.Platform[k]
	}
	return &copy
}

// Cmd includes all the information required to run a build command.
type Cmd struct {
	// ID is used as a unique identifier for this action in logs and tracing.
	// It does not have to be human-readable, so using a UUID is fine.
	ID string

	// Desc is a short, human-readable identifier that is shown to the user when referencing this action in the UI or a log file.
	// Example: "CXX hello.o"
	Desc string

	// ActionName is the name of the rule that generated this action.
	// Example: "cxx" or "link"
	ActionName string

	// Args holds command line arguments.
	Args []string

	// Env specifies the environment of the process.
	Env []string

	// RSPFile is the filename of the response file for the cmd.
	// If set,  Siso will write the RSPFileContent to the file before executing the action, and delete the file after executing the cmd successfully.
	RSPFile string

	// RSPFileContent is the content of the response file for the cmd.
	// The bindings are already expanded.
	RSPFileContent []byte

	// CmdHash is a hash of the command line, which is used to check for changes in the command line since it was last executed.
	CmdHash []byte

	// EdgeHash is a hash of the inputs/outputs paths, which is used to check for changes in the inputs/outputs since it was last executed.
	EdgeHash []byte

	// ExecRoot is an exec root directory of the cmd.
	ExecRoot string

	// Dir specifies the working directory of the cmd,
	// relative to ExecRoot.
	Dir string

	// Inputs are input files of the cmd, relative to ExecRoot.
	// They may be overridden by deps inputs.
	Inputs []string

	// ToolInputs are tool input files of the cmd, relative to ExecRoot.
	// They are specified by the siso config, not overridden by deps.
	// (or inputs would be deps + tool inputs).
	// These are expected to be toolchain input files, not by specified
	// by build deps, nor in deps log.
	ToolInputs []string

	// TreeInputs are precomputed subtree inputs of the cmd.
	TreeInputs []merkletree.TreeEntry

	// If UseSystemInputs is true, inputs may include system includes,
	// but it won't be included in remote exec request and expect such
	// files exist in platform container image.
	UseSystemInput bool

	// Outputs are output files of the cmd, relative to ExecRoot.
	Outputs []string

	// ReconcileOutputdirs are output directories where the cmd would
	// modify files/dirs, invisible to build graph.
	ReconcileOutputdirs []string

	// Deps specifies deps type of the cmd, "gcc", "msvc".
	Deps string

	// Depfile specifies a filename for dep info, relative to ExecRoot.
	Depfile string

	// If Restat is true,
	// output files may be used only for inputs. i.e.
	// output files would not be produced if they would be the same
	// as before by local command execution, so mtime would not be
	// updated, but UpdateTime is updated and IsChagned becomes true.
	Restat bool

	// If RestatContent is true, works as if reset=true for content.
	// i.e. if output content is the same as before, don't update mtime
	// but update UpdateTime and IsChagned to be false.
	RestatContent bool

	// Pure indicates whether the cmd is pure.
	// This is analogue to pure function.
	// For example, a cmd is pure when the inputs/outputs of the cmd are fully specified,
	// and it doesn't access other files during execution.
	// A pure cmd can execute remotely and the outputs can be safely cacheable.
	Pure bool

	// SkipCacheLookup specifies it won't lookup cache in remote execution.
	SkipCacheLookup bool

	// HashFS is a hash fs that the cmd runs on.
	HashFS *hashfs.HashFS

	// REAPI version
	REAPIVersion *semverpb.SemVer

	// Platform is a platform properties for remote execution.
	// e.g. OSFamily: {Linux, Windows}
	Platform map[string]string

	// RemoteWrapper is a wrapper command when the cmd runs on remote execution backend.
	// It can be used to specify a wrapper command/script that exist on the worker.
	RemoteWrapper string

	// RemoteCommand is an argv[0] when the cmd runs on remote execution backend, if not empty.
	// e.g. "python3" for python actions sent from Windows host to Linux worker.
	RemoteCommand string

	// RemoteInputs are the substitute files for remote execution.
	// The key is the filename used in remote execution.
	// The value is the filename on local disk.
	// The file names are relative to ExecRoot.
	RemoteInputs map[string]string

	// REProxyConfig specifies configuration options for using reproxy.
	// If using reproxy, this config takes precedence over options in this struct.
	REProxyConfig *REProxyConfig

	// CanonicalizeDir specifies whether remote execution will canonicalize
	// working directory or not.
	CanonicalizeDir bool

	// DoNotCache specifies whether it won't update cache in remote execution.
	DoNotCache bool

	// Timeout specifies timeout of the cmd, applicable only to the remote execution strategy.
	Timeout time.Duration

	// ExecTimeout specifies exec timeout of the cmd, applicable only to the remote execution strategy.
	ExecTimeout time.Duration

	// ActionSalt is arbitrary bytes used for cache salt.
	ActionSalt []byte

	// Console indicates the command attaches stdin/stdout/stderr when
	// running.  localexec only.
	Console bool

	// ConsoleOut indicates the command outputs to the console.
	ConsoleOut *atomic.Bool

	// OOMScoreAdj is value to set oom_score_adj on local exec (linux only)
	OOMScoreAdj *int

	// outfiles is outputs of the step in build graph.
	// These outputs will be recorded with cmdhash.
	// Other outputs in c.Outputs will be recorded without cmdhash.
	outfiles map[string]bool

	// preOutputEntries is update entries of outputs before execution.
	preOutputEntries []hashfs.UpdateEntry

	stdoutBuffer, stderrBuffer *bytes.Buffer

	actionDigest digest.Digest

	actionResult *rpb.ActionResult

	// actionResult is cached result if cachedResult is true.
	cachedResult bool

	// remoteFallbackResult is rpb.ActionResult from remote execution.
	// This is used to distinguish from actionResult when local fallback happens.
	remoteFallbackResult *rpb.ActionResult

	// remoteFallbackError is an error returned while running remote execution.
	// This is used to distinguish from an error when local fallback happens.
	remoteFallbackError error

	outputResult string
}

// String returns an ID of the cmd.
func (c *Cmd) String() string {
	return c.ID
}

// InitOutputs initializes outputs for the cmd.
// c.Outputs will be recorded as outputs of the command in hashfs
// in RecordOutputs or RecordOutputsFromLocal.
func (c *Cmd) InitOutputs() {
	c.outfiles = make(map[string]bool)
	for _, out := range c.Outputs {
		c.outfiles[out] = true
	}
}

// AllInputs returns all inputs of the cmd.
func (c *Cmd) AllInputs() []string {
	if c.RSPFile == "" {
		return c.Inputs
	}
	inputs := make([]string, len(c.Inputs)+1)
	copy(inputs, c.Inputs)
	inputs[len(inputs)-1] = c.RSPFile
	return inputs
}

// AllOutputs returns all outputs of the cmd.
func (c *Cmd) AllOutputs() []string {
	if c.Depfile == "" {
		return c.Outputs
	}
	outputs := make([]string, len(c.Outputs)+1)
	copy(outputs, c.Outputs)
	outputs[len(outputs)-1] = c.Depfile
	return outputs
}

// remoteArgsWithWrapper returns arguments to the remote command.
// The original args are adjusted with RemoteWrapper, RemoteCommand, Platform.
// TODO: b/379584977 - Merge remoteArgsWithWrapper and RemoteArgs when dropping
// Reproxy integration. We have two similar methods because RemoteWrapper needs
// to be passed to Reproxy as a different field to distinguish between remote
// and local commands.
func (c *Cmd) remoteArgsWithWrapper() ([]string, error) {
	args, err := c.RemoteArgs()
	if err != nil {
		return nil, err
	}
	if c.RemoteWrapper != "" {
		args = append([]string{c.RemoteWrapper}, args...)
	}
	return args, nil
}

// RemoteArgs returns arguments to the remote command.
// The original args are adjusted with RemoteCommand, Platform.
func (c *Cmd) RemoteArgs() ([]string, error) {
	args := c.Args
	if len(args) == 0 {
		return nil, errors.New("0 args")
	}
	// Cross-compile Windows builds on Linux workers.
	if runtime.GOOS == "windows" && c.Platform["OSFamily"] != "Windows" {
		args[0] = filepath.ToSlash(args[0])
		args[0] = strings.TrimPrefix(args[0], filepath.VolumeName(args[0]))
		if rootPath, ok := c.Platform["InputRootAbsolutePath"]; ok {
			rootPath = filepath.ToSlash(rootPath)
			rootPath = strings.TrimPrefix(rootPath, filepath.VolumeName(rootPath))
			c.Platform["InputRootAbsolutePath"] = rootPath
		}
	}
	if c.RemoteCommand != "" {
		// Replace the first args. But don't modify the Cmd.Args for fallback.
		args = append([]string{c.RemoteCommand}, args[1:]...)
	}
	return args, nil
}

// StdoutWriter returns a writer set for stdout.
func (c *Cmd) StdoutWriter() *bytes.Buffer {
	if c.stdoutBuffer == nil {
		c.stdoutBuffer = new(bytes.Buffer)
	}
	c.stdoutBuffer.Reset()
	return c.stdoutBuffer
}

// StderrWriter returns a writer set for stderr.
func (c *Cmd) StderrWriter() *bytes.Buffer {
	if c.stderrBuffer == nil {
		c.stderrBuffer = new(bytes.Buffer)
	}
	c.stderrBuffer.Reset()
	return c.stderrBuffer
}

// Stdout returns stdout output of the cmd.
func (c *Cmd) Stdout() []byte {
	if c.stdoutBuffer == nil {
		return nil
	}
	return c.stdoutBuffer.Bytes()
}

// Stderr returns stderr output of the cmd.
// Since RBE merges stderr into stdout, we won't get stderr for remote actions. b/149501385
// Therefore, we need to be careful how we use stdout/stderr for now.
// For example, if we use /showIncludes to stderr, it will be on stdout from a remote action.
func (c *Cmd) Stderr() []byte {
	if c.stderrBuffer == nil {
		return nil
	}
	return c.stderrBuffer.Bytes()
}

// ActionDigest returns action digest of the cmd.
func (c *Cmd) ActionDigest() digest.Digest {
	return c.actionDigest
}

// SetActionDigest sets action digest.
// This is used to set the digest provided by Reproxy.
func (c *Cmd) SetActionDigest(d digest.Digest) {
	c.actionDigest = d
}

// RemoteChroot returns whether it is executed under chroot on remote worker.
// e.g. dockerChrootPath=. in platform property.
func (c *Cmd) RemoteChroot() bool {
	_, ok := c.Platform["dockerChrootPath"]
	return ok
}

// Digest computes action digest of the cmd.
// If ds is nil, then it will reuse the previous calculated digest if any.
func (c *Cmd) Digest(ctx context.Context, ds *digest.Store) (actionDigest digest.Digest, err error) {
	if !c.Pure {
		return digest.Digest{}, fmt.Errorf("unable to create digest for impure cmd %s", c.ID)
	}
	if c.HashFS == nil {
		return digest.Digest{}, fmt.Errorf("unable to get the input root for %s: missing HashFS", c)
	}
	chrootPath, remoteChroot := c.Platform["dockerChrootPath"]
	if remoteChroot {
		if chrootPath != "." {
			return digest.Digest{}, fmt.Errorf("unsupported dockerChrootPath=%q", chrootPath)
		}
	}
	var inputRootDigest, commandDigest digest.Digest
	var treeDuration time.Duration
	defer func() {
		// -2 for command and action message.
		clog.Infof(ctx, "action: %s {command; %s inputRoot: %s %d %s}: %v", actionDigest, commandDigest, inputRootDigest, ds.Size()-2, treeDuration, err)
	}()
	started := time.Now()
	ents, err := c.inputTree(ctx)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to get input tree for %s: %w", c, err)
	}

	treeInputs := c.TreeInputs
	if c.CanonicalizeDir {
		ents, treeInputs = c.canonicalizeDir(ctx, ents, treeInputs)
	}
	if remoteChroot {
		ents, treeInputs = c.chrootDir(ctx, ents, treeInputs)
	}

	inputRootDigest, err = treeDigest(ctx, treeInputs, ents, ds)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to get input root for %s: %w", c, err)
	}
	treeDuration = time.Since(started)

	commandDigest, err = c.commandDigest(ctx, ds)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to build command for %s: %w", c, err)
	}

	var timeout *durationpb.Duration
	if c.ExecTimeout > 0 {
		// If ExecTimeout is specified explicitly, use it.
		timeout = durationpb.New(c.ExecTimeout)
	} else if c.Timeout > 0 {
		// Set Timeout*2 to expect cache hit for long command.
		// but prevent from keeping RBE worker busy.
		timeout = durationpb.New(c.Timeout * 2)
	}

	actionMsg := &rpb.Action{
		CommandDigest:   commandDigest.Proto(),
		InputRootDigest: inputRootDigest.Proto(),
		Timeout:         timeout,
		DoNotCache:      c.DoNotCache,
		Salt:            c.ActionSalt,
	}
	if reapi.UseActionForPlatformProperties(c.REAPIVersion) {
		actionMsg.Platform = c.remoteExecutionPlatform()
	}
	action, err := digest.FromProtoMessage(actionMsg)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to build action for %s: %w", c, err)
	}
	if ds != nil {
		ds.Set(action)
	}
	c.actionDigest = action.Digest()
	return c.actionDigest, nil
}

// inputTree returns Merkle tree entries for the cmd.
func (c *Cmd) inputTree(ctx context.Context) ([]merkletree.Entry, error) {
	inputs := c.AllInputs()
	if log.V(1) {
		clog.Infof(ctx, "tree @%s %s", c.ExecRoot, inputs)
	}
	var rootEnts []merkletree.Entry
	switch {
	case c.RemoteChroot():
		// allow absolute path for inputs when remote chroot.
		var rootInputs, newInputs []string
		for _, input := range inputs {
			if !filepath.IsLocal(input) {
				if filepath.IsAbs(input) {
					rootInputs = append(rootInputs, input)
					continue
				}
				rootInputs = append(rootInputs, filepath.ToSlash(filepath.Join(c.ExecRoot, input)))
				continue
			}
			newInputs = append(newInputs, input)
		}
		if len(rootInputs) > 0 {
			var err error
			// use "" as root as rootInputs are absolute paths.
			rootEnts, err = c.HashFS.Entries(ctx, "", rootInputs)
			if err != nil {
				return nil, fmt.Errorf("failed to get entries for remote chroot inputs: %w", err)
			}
			clog.Infof(ctx, "external inputs %d -> %d", len(rootInputs), len(rootEnts))
		}
		inputs = newInputs

	case c.UseSystemInput:
		var newInputs []string
		for _, input := range inputs {
			if !filepath.IsLocal(input) {
				continue
			}
			newInputs = append(newInputs, input)
		}
		clog.Infof(ctx, "drop %d system inputs -> %d", len(inputs)-len(newInputs), len(newInputs))
		inputs = newInputs
	}

	ents, err := c.HashFS.Entries(ctx, c.ExecRoot, inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to get entries for inputs in %s: %w", c.ExecRoot, err)
	}
	ents = append([]merkletree.Entry{{Name: c.Dir}}, ents...)
	ents = append(ents, rootEnts...)

	if len(c.RemoteInputs) == 0 {
		sort.Slice(ents, func(i, j int) bool {
			return ents[i].Name < ents[j].Name
		})
		return ents, nil
	}
	if log.V(1) {
		clog.Infof(ctx, "remote tree @%s %s", c.ExecRoot, c.RemoteInputs)
	}

	// Construct a reverse map from local path to remote paths.
	// Note that multiple remote inputs may use the same local input.
	// Also, make a list of local filepaths to retrieve entries from the HashFS.
	revm := map[string][]string{}
	reins := make([]string, 0, len(c.RemoteInputs))
	for r, l := range c.RemoteInputs {
		reins = append(reins, l)
		revm[l] = append(revm[l], r)
	}

	// Retrieve Merkle tree entries from HashFS.
	sort.Strings(reins)
	reents, err := c.HashFS.Entries(ctx, c.ExecRoot, reins)
	if err != nil {
		return nil, fmt.Errorf("failed to get entries for remote inputs in %s: %w", c.ExecRoot, err)
	}

	// Convert local paths to remote paths.
	remap := map[string]merkletree.Entry{}
	for _, e := range reents {
		for _, rname := range revm[e.Name] {
			e.Name = rname
			remap[rname] = e
		}
	}

	// Replace local entries with the remote entries.
	for i, e := range ents {
		re, ok := remap[e.Name]
		if ok {
			ents[i] = re
			delete(remap, e.Name)
		}
	}

	// Append the remaining remote entries.
	for _, re := range remap {
		ents = append(ents, re)
	}

	sort.Slice(ents, func(i, j int) bool {
		return ents[i].Name < ents[j].Name
	})
	return ents, nil
}

// treeDigest returns a digest for the Merkle tree entries.
func treeDigest(ctx context.Context, subtrees []merkletree.TreeEntry, entries []merkletree.Entry, ds *digest.Store) (digest.Digest, error) {
	t := merkletree.New(ds)
	for _, subtree := range subtrees {
		if log.V(2) {
			clog.Infof(ctx, "input subtree: %#v", subtree)
		}
		err := t.SetTree(subtree)
		if errors.Is(err, merkletree.ErrPrecomputedSubTree) {
			// probably wrong TreeInputs are set.
			// assume upper subtree covers lower subtree,
			// so ignore ErrPrecomputedSubTree here.
			clog.Warningf(ctx, "ignore subtree %v: %v", subtree, err)
			continue
		}
		if err != nil {
			return digest.Digest{}, err
		}
	}
	for _, ent := range entries {
		if log.V(2) {
			clog.Infof(ctx, "input entry: %#v", ent)
		}
		err := t.Set(ent)
		if errors.Is(err, merkletree.ErrPrecomputedSubTree) {
			// wrong config or deps uses files in subtree.
			// assume subtree contains the file,
			// so ignore ErrPrecomputedSubTree here.
			if log.V(1) {
				clog.Warningf(ctx, "ignore entry in subtree %v: %v", ent, err)
			}
			continue
		}
		if err != nil {
			return digest.Digest{}, err
		}
	}

	d, err := t.Build(ctx)
	if err != nil {
		return digest.Digest{}, err
	}
	return d, nil
}

// canonicalizeDir canonicalizes working dir in the entries and trees.
func (c *Cmd) canonicalizeDir(ctx context.Context, ents []merkletree.Entry, treeInputs []merkletree.TreeEntry) ([]merkletree.Entry, []merkletree.TreeEntry) {
	cdir := c.canonicalDir()
	if cdir == "" {
		return ents, treeInputs
	}
	if log.V(1) {
		clog.Infof(ctx, "canonicalize dir: %s -> %s", c.Dir, cdir)
	}
	ents = c.canonicalizeEntries(cdir, ents)
	treeInputs = slices.Clone(treeInputs)
	treeInputs = c.canonicalizeTrees(cdir, treeInputs)
	return ents, treeInputs
}

// canonicalizeEntries canonicalizes working dir to cdir in the entries.
func (c *Cmd) canonicalizeEntries(cdir string, entries []merkletree.Entry) []merkletree.Entry {
	for i := range entries {
		e := &entries[i]
		e.Name = canonicalizeDir(e.Name, c.Dir, cdir)
	}
	return entries
}

// canonicalizeTrees canonicalizes working dir to cdir in the trees.
func (c *Cmd) canonicalizeTrees(cdir string, trees []merkletree.TreeEntry) []merkletree.TreeEntry {
	for i := range trees {
		e := &trees[i]
		e.Name = canonicalizeDir(e.Name, c.Dir, cdir)
	}
	return trees
}

// canonicalDir computes a canonical dir of the working directory.
func (c *Cmd) canonicalDir() string {
	if c.Dir == "" || c.Dir == "." {
		return ""
	}
	n := len(strings.Split(filepath.ToSlash(c.Dir), "/"))
	elems := []string{"out"}
	for i := 1; i < n; i++ {
		elems = append(elems, "x")
	}
	return filepath.ToSlash(filepath.Join(elems...))
}

func canonicalizeDir(fname, dir, cdir string) string {
	if dir == cdir {
		return fname
	}
	if fname == dir {
		return cdir
	}
	for _, prefix := range []string{dir + "/", dir + `\`} {
		if f, ok := strings.CutPrefix(fname, prefix); ok {
			return filepath.ToSlash(filepath.Join(cdir, f))
		}
	}
	return fname
}

// chrootDir converts pathnames in ents and treeInputs from exec root relative to "/" relative.
func (c *Cmd) chrootDir(ctx context.Context, ents []merkletree.Entry, treeInputs []merkletree.TreeEntry) ([]merkletree.Entry, []merkletree.TreeEntry) {
	dir := filepath.ToSlash(filepath.Clean(c.ExecRoot))
	if log.V(1) {
		clog.Infof(ctx, "chdoor dir: %s", dir)
	}
	for i := range ents {
		e := &ents[i]
		if filepath.IsAbs(e.Name) {
			e.Name = e.Name[1:]
			continue
		}
		e.Name = filepath.ToSlash(filepath.Join(dir, e.Name))[1:]
	}
	treeInputs = slices.Clone(treeInputs)
	for i := range treeInputs {
		e := &treeInputs[i]
		if filepath.IsAbs(e.Name) {
			e.Name = e.Name[1:]
			continue
		}
		e.Name = filepath.ToSlash(filepath.Join(dir, e.Name))[1:]
	}
	return ents, treeInputs
}

// remoteExecutionPlatform constructs a Remote Execution Platform properties from the platform properties.
func (c *Cmd) remoteExecutionPlatform() *rpb.Platform {
	platform := &rpb.Platform{}
	for k, v := range c.Platform {
		platform.Properties = append(platform.Properties, &rpb.Platform_Property{
			Name:  k,
			Value: v,
		})
	}
	sort.Slice(platform.Properties, func(i, j int) bool {
		return platform.Properties[i].Name < platform.Properties[j].Name
	})
	return platform
}

// commandDigest constructs the digest of the command line.
func (c *Cmd) commandDigest(ctx context.Context, ds *digest.Store) (digest.Digest, error) {
	outputs := c.AllOutputs()
	outs := make([]string, 0, len(outputs))
	for _, out := range outputs {
		rout, err := filepath.Rel(c.Dir, out)
		if err != nil {
			clog.Warningf(ctx, "failed to get rel %s,%s: %v", c.Dir, out, err)
			rout = out
		}
		outs = append(outs, filepath.ToSlash(rout))
	}
	sort.Strings(outs)
	args, err := c.remoteArgsWithWrapper()
	if err != nil {
		return digest.Digest{}, err
	}
	dir := c.Dir
	if c.CanonicalizeDir {
		dir = c.canonicalDir()
	}
	if c.RemoteChroot() {
		dir = filepath.ToSlash(filepath.Join(c.ExecRoot, dir))[1:]
	}
	// out files are cwd relative.
	command := &rpb.Command{
		Arguments:        args,
		WorkingDirectory: filepath.ToSlash(dir),
		// TODO(b/273152496): `Platform` in `Command` is deprecated. should specify it in `Action`.
		// https://github.com/bazelbuild/remote-apis/blob/55153ba61dcf6277849562a30bca9fa3906ad9a0/build/bazel/remote/execution/v2/remote_execution.proto#L661-L664
		// https://github.com/bazelbuild/remote-apis/blob/55153ba61dcf6277849562a30bca9fa3906ad9a0/build/bazel/remote/execution/v2/remote_execution.proto#L519-L521
		// clients SHOULD set these platform properties as well
		// as those in the Command.
		Platform: c.remoteExecutionPlatform(), // deprecated?
	}
	// `OutputFiles` is deprecated. should use `OutputPaths` instead.
	// https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L592
	if reapi.UseOutputPaths(c.REAPIVersion) {
		command.OutputPaths = outs
	} else {
		command.OutputFiles = outs
	}
	for _, env := range c.Env {
		k, v, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		command.EnvironmentVariables = append(command.EnvironmentVariables, &rpb.Command_EnvironmentVariable{
			Name:  k,
			Value: v,
		})
	}
	sort.Slice(command.EnvironmentVariables, func(i, j int) bool {
		return command.EnvironmentVariables[i].Name < command.EnvironmentVariables[j].Name
	})
	data, err := digest.FromProtoMessage(command)
	if err != nil {
		return digest.Digest{}, err
	}
	if ds != nil {
		ds.Set(data)
	}
	return data.Digest(), nil
}

// SetActionResult sets action result to the cmd.
func (c *Cmd) SetActionResult(result *rpb.ActionResult, cached bool) {
	c.actionResult = result
	c.cachedResult = cached
}

// ActionResult returns the action result of the cmd.
func (c *Cmd) ActionResult() (*rpb.ActionResult, bool) {
	return c.actionResult, c.cachedResult
}

// SetRemoteFallbackResult sets remote action failed result and remote error of the cmd, which ended up with local fallback.
func (c *Cmd) SetRemoteFallbackResult(result *rpb.ActionResult, err error) {
	c.remoteFallbackResult = result
	c.remoteFallbackError = err
}

// RemoteFallbackResult returns the remote action failed result and remote error of the cmd, which ended up with local fallback.
func (c *Cmd) RemoteFallbackResult() (*rpb.ActionResult, error) {
	return c.remoteFallbackResult, c.remoteFallbackError
}

// entriesFromResult returns output file entries and additional entries for the cmd and result.
// output file entries will be recorded with cmdhash.
// additional output file entries will be recorded without cmdhash.
func (c *Cmd) entriesFromResult(ctx context.Context, ds hashfs.DataSource, updatedTime time.Time) (entries, additionalEntries []hashfs.UpdateEntry) {
	for _, f := range c.actionResult.GetOutputFiles() {
		if f.Digest == nil {
			continue
		}
		fname := filepath.ToSlash(filepath.Join(c.Dir, f.Path))
		d := digest.FromProto(f.Digest)
		mode := fs.FileMode(0644)
		if f.IsExecutable {
			mode |= 0111
		}
		ent := hashfs.UpdateEntry{
			Name: fname,
			Entry: &merkletree.Entry{
				Name:         fname,
				Data:         digest.NewData(ds.Source(ctx, d, fname), d),
				IsExecutable: f.IsExecutable,
			},
			Mode:        mode,
			ModTime:     updatedTime,
			Action:      c.actionDigest,
			UpdatedTime: updatedTime,
			IsChanged:   true,
		}
		if !c.outfiles[fname] {
			// don't set cmdhash
			additionalEntries = append(additionalEntries, ent)
			continue
		}
		ent.CmdHash = c.CmdHash
		entries = append(entries, ent)
	}
	for _, s := range c.actionResult.GetOutputSymlinks() {
		if s.Target == "" {
			continue
		}
		fname := filepath.ToSlash(filepath.Join(c.Dir, s.Path))
		mode := fs.FileMode(0644) | fs.ModeSymlink
		entries = append(entries, hashfs.UpdateEntry{
			Name: fname,
			Entry: &merkletree.Entry{
				Name:   fname,
				Target: s.Target,
			},
			Mode:        mode,
			ModTime:     updatedTime,
			CmdHash:     c.CmdHash,
			Action:      c.actionDigest,
			UpdatedTime: updatedTime,
			IsChanged:   true,
		})
	}
	for _, d := range c.actionResult.GetOutputDirectories() {
		// It just needs to add the directories here because it assumes that they have already been expanded by ninja State.
		dname := filepath.ToSlash(filepath.Join(c.Dir, d.Path))
		mode := fs.FileMode(0755) | fs.ModeDir
		entries = append(entries, hashfs.UpdateEntry{
			Name: dname,
			Entry: &merkletree.Entry{
				Name: dname,
			},
			Mode:        mode,
			ModTime:     updatedTime,
			CmdHash:     c.CmdHash,
			Action:      c.actionDigest,
			UpdatedTime: updatedTime,
			IsChanged:   true,
		})
	}
	return entries, additionalEntries
}

// RecordPreOutputs records output entries before running command.
// hashfs would lazily compute digest of files, so it would
// cause ERROR_SHARING_VIOLATION when running command on Windows.
// to prevent the error, compute digest before running step.
// TODO: use this to enable restat for remote execution.
func (c *Cmd) RecordPreOutputs(ctx context.Context) {
	c.preOutputEntries = c.HashFS.RetrieveUpdateEntries(ctx, c.ExecRoot, c.AllOutputs())
}

// RecordOutputs records cmd's outputs from action result in hashfs.
func (c *Cmd) RecordOutputs(ctx context.Context, ds hashfs.DataSource, now time.Time) error {
	entries, additionalEntries := c.entriesFromResult(ctx, ds, now)
	clog.Infof(ctx, "output entries %d+%d", len(entries), len(additionalEntries))
	entries = c.computeOutputEntries(entries, now, c.CmdHash)
	err := c.HashFS.Update(ctx, c.ExecRoot, entries)
	if err != nil {
		return fmt.Errorf("failed to update hashfs from remote: %w", err)
	}
	if len(additionalEntries) == 0 {
		return nil
	}
	additionalEntries = c.computeOutputEntries(additionalEntries, now, nil)
	err = c.HashFS.Update(ctx, c.ExecRoot, additionalEntries)
	if err != nil {
		return fmt.Errorf("failed to update hashfs from remote[additional]: %w", err)
	}
	return nil
}

func retrieveLocalOutputEntries(ctx context.Context, hfs *hashfs.HashFS, root string, inputs []string) []hashfs.UpdateEntry {
	return hfs.RetrieveUpdateEntriesFromLocal(ctx, root, inputs)
}

func updateLocalOutputDir(ctx context.Context, hfs *hashfs.HashFS, root, dir string, outputs map[string]hashfs.UpdateEntry) (err error) {
	started := time.Now()
	defer func() {
		if err != nil {
			clog.Warningf(ctx, "failed to update local output dir %q: %v", dir, err)
		} else {
			clog.Infof(ctx, "update local output dir %q: %s", dir, time.Since(started))
		}
	}()

	entriesFromLocalDir := func(dir string) ([]hashfs.UpdateEntry, error) {
		dents, err := hfs.ReadDir(ctx, root, dir)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(dents))
		for _, dent := range dents {
			fname := filepath.ToSlash(filepath.Join(dir, dent.Name()))
			if _, ok := outputs[fname]; ok {
				continue
			}
			names = append(names, fname)
		}
		if len(names) == 0 {
			return nil, nil
		}
		return hfs.RetrieveUpdateEntriesFromLocal(ctx, root, names), nil
	}
	ents, err := entriesFromLocalDir(dir)
	if err != nil {
		return err
	}
	for i := 0; i < len(ents); i++ {
		ent := ents[i]
		if !ent.Mode.IsDir() {
			continue
		}
		subdirEnts, err := entriesFromLocalDir(ent.Name)
		if err != nil {
			return err
		}
		ents = append(ents, subdirEnts...)
	}
	if len(ents) == 0 {
		return nil
	}
	return hfs.Update(ctx, root, ents)
}

// computeOutputEntries computes output entries to have updatedTime and cmdhash.
// if c.Restat or c.ResetContent is true, it checks preOutputEntries recorded
// by RecordPreOutputs and don't update mtime/is_changed
// if entry is the same as before.
func (c *Cmd) computeOutputEntries(entries []hashfs.UpdateEntry, updatedTime time.Time, cmdhash []byte) []hashfs.UpdateEntry {
	pre := make(map[string]hashfs.UpdateEntry)
	if c.Restat || c.RestatContent {
		// check with previous content recorded by
		// RecordPreOutputs before execution.
		for _, ent := range c.preOutputEntries {
			pre[ent.Name] = ent
		}
	}

	ret := make([]hashfs.UpdateEntry, 0, len(entries))
	// Set cmdhash, updatedTime.
	// also isChanged=true if entry has been changed.
	for i, ent := range entries {
		ent.CmdHash = cmdhash
		if i == 0 || ent.Name == c.Outputs[0] {
			ent.EdgeHash = c.EdgeHash
		}
		ent.UpdatedTime = updatedTime
		pent := pre[ent.Name]
		switch {
		case c.Restat && ent.IsLocal:
			ent.IsChanged = !pent.ModTime.Equal(ent.ModTime)

		case c.RestatContent:
			ent.IsChanged = true
			if pent.Entry != nil && !pent.Entry.Data.IsZero() && ent.Entry != nil && !ent.Entry.Data.IsZero() {
				// empty file (e.g. stamp file) always
				// considered as changed
				ent.IsChanged = pent.Entry.Data.Digest() != ent.Entry.Data.Digest() || ent.Entry.Data.Digest().SizeBytes == 0
			}
			if ent.IsChanged {
				ent.ModTime = updatedTime
			} else {
				ent.ModTime = pent.ModTime
			}
		default:
			ent.ModTime = updatedTime
			ent.IsChanged = true
		}
		ret = append(ret, ent)
	}
	return ret
}

// RecordOutputsFromLocal records cmd's outputs from local disk in hashfs.
func (c *Cmd) RecordOutputsFromLocal(ctx context.Context, now time.Time) error {
	for _, dir := range c.ReconcileOutputdirs {
		c.HashFS.ForgetMissingsInDir(ctx, c.ExecRoot, dir)
	}
	var additionalFiles []string
	if c.Depfile != "" && !c.outfiles[c.Depfile] {
		additionalFiles = append(additionalFiles, c.Depfile)
	}
	for _, out := range c.Outputs {
		if !c.outfiles[out] {
			additionalFiles = append(additionalFiles, out)
		}
	}
	if len(additionalFiles) > 0 {
		sort.Strings(additionalFiles)
		entries := retrieveLocalOutputEntries(ctx, c.HashFS, c.ExecRoot, additionalFiles)
		entries = c.computeOutputEntries(entries, now, nil)
		err := c.HashFS.Update(ctx, c.ExecRoot, entries)
		if err != nil {
			return fmt.Errorf("failed to update hashfs from local[additional]: %w", err)
		}
	}
	var outs []string
	for out := range c.outfiles {
		outs = append(outs, out)
	}
	sort.Strings(outs)
	entries := retrieveLocalOutputEntries(ctx, c.HashFS, c.ExecRoot, outs)
	entries = c.computeOutputEntries(entries, now, c.CmdHash)
	err := c.HashFS.Update(ctx, c.ExecRoot, entries)
	if err != nil {
		return fmt.Errorf("failed to update hashfs from local: %w", err)
	}
	outputs := make(map[string]hashfs.UpdateEntry)
	for _, ent := range entries {
		outputs[ent.Name] = ent
	}
	for _, ent := range entries {
		if !ent.Mode.IsDir() {
			continue
		}
		// If output is dir, forget non-outputs under dir and retrieve
		// from local disk.
		err := updateLocalOutputDir(ctx, c.HashFS, c.ExecRoot, ent.Name, outputs)
		if err != nil {
			return fmt.Errorf("failed to update hashfs from local dir %q: %w", ent.Name, err)
		}
	}
	if c.Restat {
		pre := make(map[string]hashfs.UpdateEntry)
		for _, ent := range c.preOutputEntries {
			pre[ent.Name] = ent
		}
		ents := c.HashFS.RetrieveUpdateEntries(ctx, c.ExecRoot, outs)
		// log restat mtime updated same content, which would
		// differ in mtime-less build.
		// TODO: remove when mtime-based build is deprecated.
		for _, ent := range ents {
			if !ent.IsChanged {
				continue
			}
			pent := pre[ent.Name]
			if pent.ModTime.Equal(ent.ModTime) {
				continue
			}
			if pent.Entry == nil || ent.Entry == nil {
				continue
			}
			d := ent.Entry.Data.Digest()
			if pent.Entry.Data.Digest() != d {
				continue
			}
			if d.SizeBytes == 0 {
				clog.Warningf(ctx, "restat: empty file %q %s->%s", ent.Name, pent.ModTime, ent.ModTime)
				continue
			}
			clog.Warningf(ctx, "restat: changed but not modified %q %s %s->%s", ent.Name, ent.Entry.Data.Digest(), pent.ModTime, ent.ModTime)
		}
	}
	return nil
}

// ResultFromEntries updates result from entries (collected from exec root).
func ResultFromEntries(ctx context.Context, result *rpb.ActionResult, dir string, entries []merkletree.Entry) {
	for _, ent := range entries {
		name, err := filepath.Rel(dir, ent.Name)
		if err != nil {
			clog.Warningf(ctx, "failed to get rel path %q: %v", ent.Name, err)
			continue
		}
		name = filepath.ToSlash(name)
		switch {
		case ent.IsSymlink():
			result.OutputSymlinks = append(result.OutputSymlinks, &rpb.OutputSymlink{
				Path:   name,
				Target: ent.Target,
			})
		case ent.IsDir():
			result.OutputDirectories = append(result.OutputDirectories, &rpb.OutputDirectory{
				Path: name,
				// TODO(b/275448031): calculate tree digest from the entry.
				TreeDigest: digest.Empty.Proto(),
			})
		default:
			result.OutputFiles = append(result.OutputFiles, &rpb.OutputFile{
				Path:         name,
				Digest:       ent.Data.Digest().Proto(),
				IsExecutable: ent.IsExecutable,
			})
		}
	}
}

// ExitError is an error of cmd exit.
type ExitError struct {
	ExitCode int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("exit=%d", e.ExitCode)
}

// SetOutputResult sets output result.
func (c *Cmd) SetOutputResult(msg string) {
	c.outputResult = msg
}

// SetOutputResult sets output result.
func (c *Cmd) OutputResult() string {
	return c.outputResult
}

// ExitCode returns cmd exit code, or -1.
func (c *Cmd) ExitCode() int32 {
	if c.actionResult == nil {
		return -1
	}
	return c.actionResult.ExitCode
}
