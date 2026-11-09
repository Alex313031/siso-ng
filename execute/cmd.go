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
	"os"
	stdpath "path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// Executor is an interface to run the cmd.
type Executor interface {
	Run(ctx context.Context, cmd *Cmd) error
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

	// EnvCacheKeyOmits specifies environment variable names to omit when
	// constructing RBE Command_EnvironmentVariable entries.
	EnvCacheKeyOmits map[string]bool

	// EnvCacheKeySubstitutions maps strings to their replacements when normalizing
	// environment variable values for RBE Command_EnvironmentVariable entries.
	EnvCacheKeySubstitutions map[string]string

	// EnvCacheKeyTrimPrefixes lists prefixes to strip from normalized environment
	// variable values when the value does not contain a path list separator.
	EnvCacheKeyTrimPrefixes []string

	// RSPFile is the filename of the response file for the cmd.
	// If set,  Siso will write the RSPFileContent to the file before executing the action, and delete the file after executing the cmd successfully.
	RSPFile path.Path

	// RSPFileContent is the content of the response file for the cmd.
	// The bindings are already expanded.
	RSPFileContent []byte

	// CmdHash is a hash of the command line, which is used to check for changes in the command line since it was last executed.
	CmdHash []byte

	// EdgeHash is a hash of the inputs/outputs paths, which is used to check for changes in the inputs/outputs since it was last executed.
	EdgeHash []byte

	// WorkspaceRoot is the path to the workspace of this cmd.
	WorkspaceRoot string

	// WorkDir specifies the working directory of the cmd, relative to WorkspaceRoot.
	WorkDir path.Path

	// Inputs are input files of the cmd, relative to WorkspaceRoot.
	// They may be overridden by deps inputs.
	Inputs []path.Path

	// ToolInputs are tool input files of the cmd, relative to WorkspaceRoot.
	// They are specified by the siso config, not overridden by deps.
	// (or inputs would be deps + tool inputs).
	// These are expected to be toolchain input files, not by specified
	// by build deps, nor in deps log.
	// deprecated: use scandeps.step_inputs to filter step inputs.
	ToolInputs []path.Path

	// TreeInputs are precomputed subtree inputs of the cmd.
	TreeInputs []merkletree.TreeEntry

	// If UseSystemInputs is true, inputs may include system includes,
	// but it won't be included in remote exec request and expect such
	// files exist in platform container image.
	UseSystemInput bool

	// Outputs are output files of the cmd, relative to WorkspaceRoot.
	Outputs []path.Path

	// OutputDirs are output directories of the cmd, relative to WorkspaceRoot.
	OutputDirs []path.Path

	// ReconcileOutputdirs are output directories where the cmd would
	// modify files/dirs, invisible to build graph.
	// They are relative to ExecRoot.
	ReconcileOutputdirs []path.Path

	// PostProc is post process to capture outputs from jail after sandbox
	// execution.
	PostProc func(context.Context, []path.Path) error

	// Deps specifies deps type of the cmd, "gcc", "msvc".
	Deps string

	// Depfile specifies a filename for dep info, relative to WorkspaceRoot.
	Depfile path.Path

	// AuxiliaryOutputDigests holds digests of auxiliary outputs.
	AuxiliaryOutputDigests map[string]digest.Digest

	// AuxiliaryLogOutputFiles are output files that siso explicitly logs digest of
	// but doesn't download to disk or record in hashfs time.
	// They are relative to WorkspaceRoot.
	AuxiliaryLogOutputFiles []path.Path

	// AuxiliaryLogOutputDirs are output directories that siso explicitly logs digest of
	// but doesn't download to disk or record in hashfs time.
	// They are relative to WorkspaceRoot.
	AuxiliaryLogOutputDirs []path.Path

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

	// SkipRecordOutputs skips recording outputs in hashfs after
	// remote execution.  Used in racing mode where the caller
	// records outputs after the race is decided to avoid hashfs
	// races with the local goroutine's RecordOutputsFromLocal.
	SkipRecordOutputs bool

	// HashFS is a hash fs that the cmd runs on.
	HashFS *hashfs.HashFS

	// REAPI version
	REAPIVersion reapi.Version

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
	// The file names are relative to WorkspaceRoot.
	RemoteInputs map[path.Path]path.Path

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
	outfiles map[path.Path]bool

	// preOutputEntries is update entries of outputs before execution.
	preOutputEntries []hashfs.UpdateEntry

	stdoutBuffer, stderrBuffer *bytes.Buffer

	actionDigest digest.Digest

	actionResult *rpb.ActionResult

	// dirOutputsExpanded guards expandDirOutputs against re-entry: it mutates
	// actionResult in place and a second call would double-append each tree's
	// files. Reset in Clone and SetActionResult.
	dirOutputsExpanded bool

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

// Clone creates a shallow clone of the Cmd suitable for use as
// the local racer in racing mode. All public fields are shared (they are
// read-only during execution), but private mutable state (action result,
// stdout/stderr buffers) is freshly initialized so the
// two racers don't interfere with each other.
func (c *Cmd) Clone() *Cmd {
	clone := *c // shallow copy of all fields
	clone.preOutputEntries = slices.Clone(c.preOutputEntries)
	// Reset mutable state so the clone is independent.
	clone.stdoutBuffer = nil
	clone.stderrBuffer = nil
	clone.actionDigest = digest.Digest{}
	clone.actionResult = nil
	clone.dirOutputsExpanded = false
	clone.cachedResult = false
	clone.remoteFallbackResult = nil
	clone.remoteFallbackError = nil
	clone.outputResult = ""
	clone.AuxiliaryOutputDigests = nil
	// Re-initialize outfiles map from the shared Outputs slice.
	clone.InitOutputs()
	return &clone
}

// String returns an ID of the cmd.
func (c *Cmd) String() string {
	return c.ID
}

// InitOutputs initializes outputs for the cmd.
// c.Outputs will be recorded as outputs of the command in hashfs
// in RecordOutputs or RecordOutputsFromLocal.
func (c *Cmd) InitOutputs() {
	c.outfiles = make(map[path.Path]bool)
	for _, out := range c.Outputs {
		c.outfiles[out] = true
	}
	for _, out := range c.OutputDirs {
		c.outfiles[out] = true
	}
}

// AllInputs returns all inputs of the cmd.
func (c *Cmd) AllInputs() []path.Path {
	if c.RSPFile == "" {
		return c.Inputs
	}
	inputs := make([]path.Path, len(c.Inputs)+1)
	copy(inputs, c.Inputs)
	inputs[len(inputs)-1] = c.RSPFile
	return inputs
}

// DeclaredOutputs returns the step's declared output targets: file outputs
// and directory outputs, excluding the depfile (a deps side-channel).
func (c *Cmd) DeclaredOutputs() []path.Path {
	if len(c.OutputDirs) == 0 {
		return c.Outputs
	}
	outputs := make([]path.Path, 0, len(c.Outputs)+len(c.OutputDirs))
	outputs = append(outputs, c.Outputs...)
	outputs = append(outputs, c.OutputDirs...)
	return outputs
}

// AllOutputs returns the declared outputs plus the depfile (the on-disk set
// for the capture/flush/record paths).
func (c *Cmd) AllOutputs() []path.Path {
	decl := c.DeclaredOutputs()
	if c.Depfile == "" {
		return decl
	}
	// Fresh slice: decl may alias c.Outputs, so appending must not write into
	// c.Outputs' backing array.
	outputs := make([]path.Path, 0, len(decl)+1)
	outputs = append(outputs, decl...)
	outputs = append(outputs, c.Depfile)
	return outputs
}

// FlushOutputs returns the on-disk targets to flush after the step: file
// outputs as-is, directory outputs marked with a trailing slash, plus the
// depfile. The trailing slash tells HashFS.Flush to materialize a directory
// target's whole tree; a plain file output that merely resolves to a directory
// (e.g. a legacy directory-valued "copy" output) is flushed as-is so its
// contents stay in hashfs and build-without-the-bytes is preserved.
func (c *Cmd) FlushOutputs() []path.Path {
	outputs := make([]path.Path, 0, len(c.Outputs)+len(c.OutputDirs)+1)
	outputs = append(outputs, c.Outputs...)
	for _, dir := range c.OutputDirs {
		outputs = append(outputs, dir+"/")
	}
	if c.Depfile != "" {
		outputs = append(outputs, c.Depfile)
	}
	return outputs
}

// FileOutputsWithDepfile returns the file outputs plus the depfile, excluding
// directory outputs. With no depfile it aliases c.Outputs; callers must not
// mutate the result.
func (c *Cmd) FileOutputsWithDepfile() []path.Path {
	if c.Depfile == "" {
		return c.Outputs
	}
	return append(slices.Clone(c.Outputs), c.Depfile)
}

// OutermostPaths dedups paths and drops any nested under another path in the
// set (by Path.Dir), preserving input order. Paths must use forward slashes.
// So a directory carries its whole subtree as one unit, which the jail-capture
// rename and cleandead both rely on.
func OutermostPaths(paths []path.Path) []path.Path {
	set := make(map[path.Path]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	var out []path.Path
	seen := make(map[path.Path]bool, len(paths))
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		nested := false
		for d := p.Dir(); d != "." && d != "/" && d != ""; d = d.Dir() {
			if set[d] {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, p)
		}
	}
	return out
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
		// Clone the args to avoid mutating original args that may cause
		// local exec to fail.
		args = slices.Clone(c.Args)
		args[0] = filepath.ToSlash(args[0])
		args[0] = strings.TrimPrefix(args[0], filepath.VolumeName(args[0]))
		// Platform is not used in local exec, ok to mutate here.
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
// This is used to set the digest for two phase caching or in test.
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
func (c *Cmd) Digest(ctx context.Context, ds *blob.Store) (actionDigest digest.Digest, err error) {
	if !c.Pure {
		return digest.Digest{}, fmt.Errorf("unable to create digest for impure cmd %s", c.ID)
	}
	if c.HashFS == nil {
		return digest.Digest{}, fmt.Errorf("unable to get the input root for %s: missing HashFS", c)
	}
	if ds == nil && !c.actionDigest.IsZero() {
		return c.actionDigest, nil
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

	inputRootDigest, err = treeDigest(ctx, c.HashFS.DigestFunction(), treeInputs, ents, ds)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to get input root for %s: %w", c, err)
	}
	treeDuration = time.Since(started)

	commandDigest, err = c.CommandDigest(ctx, ds)
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
	if c.REAPIVersion.UseActionForPlatformProperties() {
		actionMsg.Platform = c.remoteExecutionPlatform()
	}
	action, err := blob.FromProtoMessage(c.HashFS.DigestFunction(), actionMsg)
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
		clog.Infof(ctx, "tree @%s %s", c.WorkspaceRoot, inputs)
	}
	var rootEnts []merkletree.Entry
	switch {
	case c.RemoteChroot():
		// allow absolute path for inputs when remote chroot.
		var rootInputs []path.Path
		var newInputs []path.Path
		for _, input := range inputs {
			if !filepath.IsLocal(string(input)) {
				if filepath.IsAbs(string(input)) {
					rootInputs = append(rootInputs, input)
					continue
				}
				rootInputs = append(rootInputs, path.New(filepath.Join(c.WorkspaceRoot, string(input))))
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
		var newInputs []path.Path
		for _, input := range inputs {
			if !filepath.IsLocal(string(input)) {
				continue
			}
			newInputs = append(newInputs, input)
		}
		clog.Infof(ctx, "drop %d system inputs -> %d", len(inputs)-len(newInputs), len(newInputs))
		inputs = newInputs
	}

	inputs = c.dropSubtreeInputs(inputs)
	ents, err := c.HashFS.Entries(ctx, c.WorkspaceRoot, inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to get entries for inputs in %s: %w", c.WorkspaceRoot, err)
	}
	ents = append([]merkletree.Entry{{Name: c.WorkDir}}, ents...)
	ents = append(ents, rootEnts...)

	if len(c.RemoteInputs) == 0 {
		sort.Slice(ents, func(i, j int) bool {
			return ents[i].Name < ents[j].Name
		})
		return ents, nil
	}
	if log.V(1) {
		clog.Infof(ctx, "remote tree @%s %s", c.WorkspaceRoot, c.RemoteInputs)
	}

	// Construct a reverse map from local path to remote paths.
	// Note that multiple remote inputs may use the same local input.
	// Also, make a list of local filepaths to retrieve entries from the HashFS.
	revm := map[path.Path][]path.Path{}
	reins := make([]path.Path, 0, len(c.RemoteInputs))
	for r, l := range c.RemoteInputs {
		if strings.HasSuffix(string(l), "/") {
			// A trailing-slash directory value is expanded into its files by
			// HashFS.Entries below, after which this directory-keyed reverse
			// map no longer matches and the remap is silently dropped. Reject
			// directory values explicitly instead.
			return nil, fmt.Errorf("remote_inputs value %q for %q is a directory; directory values are not supported", l, r)
		}
		reins = append(reins, l)
		revm[l] = append(revm[l], r)
	}

	// Retrieve Merkle tree entries from HashFS.
	slices.Sort(reins)
	reents, err := c.HashFS.Entries(ctx, c.WorkspaceRoot, reins)
	if err != nil {
		return nil, fmt.Errorf("failed to get entries for remote inputs in %s: %w", c.WorkspaceRoot, err)
	}

	// Convert local paths to remote paths.
	remap := map[path.Path]merkletree.Entry{}
	for _, e := range reents {
		for _, rname := range revm[e.Name] {
			e.Name = rname
			remap[e.Name] = e
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

// dropSubtreeInputs removes inputs at or under a precomputed subtree of
// c.TreeInputs before their entries are looked up. treeDigest ignores
// such entries (Set stops at the subtree with ErrPrecomputedSubTree), so
// the input root digest is unchanged. Like SetTree, it skips a subtree
// that overlaps an earlier one. Nothing is dropped if a subtree name is
// invalid, or for remote chroot or remote inputs, which rename entries
// after the lookup.
func (c *Cmd) dropSubtreeInputs(inputs []path.Path) []path.Path {
	if len(c.TreeInputs) == 0 || c.RemoteChroot() || len(c.RemoteInputs) > 0 {
		return inputs
	}
	var cdir path.Path
	if c.CanonicalizeDir {
		cdir = c.canonicalDir()
	}
	canon := func(p path.Path) path.Path {
		if cdir == "" {
			return p
		}
		return canonicalizePath(p, c.WorkDir, cdir)
	}
	names := make([]path.Path, 0, len(c.TreeInputs))
	for _, t := range c.TreeInputs {
		n := canon(t.Name)
		if t.Digest.IsZero() || n == "." || !filepath.IsLocal(string(n)) || stdpath.Clean(string(n)) != string(n) {
			return inputs
		}
		if slices.ContainsFunc(names, func(a path.Path) bool { return n.HasPrefix(a) || a.HasPrefix(n) }) {
			continue
		}
		names = append(names, n)
	}
	under := func(p path.Path) bool {
		if cdir != "" {
			// HashFS.Entries expands a directory input "d/" to its
			// files, and those under c.WorkDir are moved out of d.
			d := path.Path(strings.TrimSuffix(string(p), "/"))
			if d != c.WorkDir && c.WorkDir.HasPrefix(d) {
				return false
			}
		}
		return slices.ContainsFunc(names, canon(p).HasPrefix)
	}
	if !slices.ContainsFunc(inputs, under) {
		return inputs
	}
	return slices.DeleteFunc(slices.Clone(inputs), under)
}

// treeDigest returns a digest for the Merkle tree entries.
func treeDigest(ctx context.Context, fn digest.Function, subtrees []merkletree.TreeEntry, entries []merkletree.Entry, ds *blob.Store) (digest.Digest, error) {
	t := merkletree.NewPooled(fn, ds)
	defer t.Release()
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
		clog.Infof(ctx, "canonicalize dir: %s -> %s", c.WorkDir, cdir)
	}
	ents = c.canonicalizeEntries(cdir, ents)
	treeInputs = slices.Clone(treeInputs)
	treeInputs = c.canonicalizeTrees(cdir, treeInputs)
	return ents, treeInputs
}

// canonicalizeEntries canonicalizes working dir to cdir in the entries.
func (c *Cmd) canonicalizeEntries(cdir path.Path, entries []merkletree.Entry) []merkletree.Entry {
	for i := range entries {
		e := &entries[i]
		e.Name = canonicalizePath(e.Name, c.WorkDir, cdir)
	}
	return entries
}

// canonicalizeTrees canonicalizes working dir to cdir in the trees.
func (c *Cmd) canonicalizeTrees(cdir path.Path, trees []merkletree.TreeEntry) []merkletree.TreeEntry {
	for i := range trees {
		e := &trees[i]
		e.Name = canonicalizePath(e.Name, c.WorkDir, cdir)
	}
	return trees
}

// canonicalDir computes a canonical dir of the working directory.
func (c *Cmd) canonicalDir() path.Path {
	if c.WorkDir == "" || c.WorkDir == "." {
		return ""
	}
	n := strings.Count(string(c.WorkDir), "/") + 1
	p := path.Path("out")
	for i := 1; i < n; i++ {
		p = p.JoinPath("x")
	}
	return p
}

func canonicalizePath(fname, dir, cdir path.Path) path.Path {
	if dir == cdir {
		return fname
	}
	if fname == dir {
		return cdir
	}
	if fname.HasPrefix(dir) {
		return cdir.JoinPath(fname.TrimPrefix(dir))
	}
	return fname
}

// chrootDir converts pathnames in ents and treeInputs from workspace relative to "/" relative.
func (c *Cmd) chrootDir(ctx context.Context, ents []merkletree.Entry, treeInputs []merkletree.TreeEntry) ([]merkletree.Entry, []merkletree.TreeEntry) {
	dir := path.New(c.WorkspaceRoot)
	if log.V(1) {
		clog.Infof(ctx, "chdoor dir: %s", dir)
	}
	for i := range ents {
		e := &ents[i]
		if e.Name.IsAbs() {
			e.Name = e.Name[1:]
			continue
		}
		e.Name = dir.JoinPath(e.Name)[1:]
	}
	treeInputs = slices.Clone(treeInputs)
	for i := range treeInputs {
		e := &treeInputs[i]
		if e.Name.IsAbs() {
			e.Name = e.Name[1:]
			continue
		}
		e.Name = dir.JoinPath(e.Name)[1:]
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

// WorkingDirectory returns working_directory for rpb.Command.
func (c *Cmd) WorkingDirectory() string {
	dir := c.WorkDir
	if c.CanonicalizeDir {
		dir = c.canonicalDir()
	}
	if dir == "." {
		dir = ""
	}
	if c.RemoteChroot() {
		dir = path.JoinRoot(c.WorkspaceRoot, dir)[1:]
	}
	return string(dir)
}

// CommandDigest constructs the digest of the command line.
func (c *Cmd) CommandDigest(ctx context.Context, ds *blob.Store) (digest.Digest, error) {
	var outFiles, outDirs []string
	process := func(res []string, paths ...path.Path) []string {
		for _, out := range paths {
			rout, err := out.Rel(c.WorkDir)
			if err != nil {
				clog.Warningf(ctx, "failed to get rel %s,%s: %v", c.WorkDir, out, err)
				res = append(res, string(out))
				continue
			}
			res = append(res, string(rout))
		}
		return res
	}

	outFiles = make([]string, 0, len(c.Outputs)+1+len(c.AuxiliaryLogOutputFiles))
	outFiles = process(outFiles, c.FileOutputsWithDepfile()...)
	outFiles = process(outFiles, c.AuxiliaryLogOutputFiles...)
	sort.Strings(outFiles)

	outDirs = process(outDirs, c.OutputDirs...)
	outDirs = process(outDirs, c.AuxiliaryLogOutputDirs...)
	sort.Strings(outDirs)
	args, err := c.remoteArgsWithWrapper()
	if err != nil {
		return digest.Digest{}, err
	}
	// out files are cwd relative.
	command := &rpb.Command{
		Arguments:        args,
		WorkingDirectory: c.WorkingDirectory(),
		// TODO(b/273152496): `Platform` in `Command` is deprecated. should specify it in `Action`.
		// https://github.com/bazelbuild/remote-apis/blob/55153ba61dcf6277849562a30bca9fa3906ad9a0/build/bazel/remote/execution/v2/remote_execution.proto#L661-L664
		// https://github.com/bazelbuild/remote-apis/blob/55153ba61dcf6277849562a30bca9fa3906ad9a0/build/bazel/remote/execution/v2/remote_execution.proto#L519-L521
		// clients SHOULD set these platform properties as well
		// as those in the Command.
		Platform: c.remoteExecutionPlatform(), // deprecated?
	}
	// `OutputFiles` is deprecated. should use `OutputPaths` instead.
	// https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L592
	if c.REAPIVersion.UseOutputPaths() {
		command.OutputPaths = make([]string, 0, len(outFiles)+len(outDirs))
		command.OutputPaths = append(command.OutputPaths, outFiles...)
		command.OutputPaths = append(command.OutputPaths, outDirs...)
		sort.Strings(command.OutputPaths)
	} else {
		command.OutputFiles = outFiles      //nolint:staticcheck // existing deprecation
		command.OutputDirectories = outDirs //nolint:staticcheck // existing deprecation
	}
	// For remote execution (RBE), c.Env only contains step-level environment variables
	// (such as macOS SDK/Xcode paths), and c.EnvCacheKeyOmits / c.EnvCacheKeySubstitutions
	// are nil. For local execution (Builder.runLocal), c.Env, c.EnvCacheKeyOmits, and
	// c.EnvCacheKeySubstitutions are populated from the "env" config, and rpb.Command is
	// only constructed to compute cache keys (e.g. two-phase caching lookup key and
	// Phase 2 action upload), never for remote execution requests.
	command.EnvironmentVariables = c.EnvVars()
	data, err := blob.FromProtoMessage(c.HashFS.DigestFunction(), command)
	if err != nil {
		return digest.Digest{}, err
	}
	if ds != nil {
		ds.Set(data)
	}
	return data.Digest(), nil
}

// NormalizeEnvVars returns a map of environment variables that have been normalized
// using the given substitutions (e.g., replacing the workspace root with "CWD").
// Any matching prefix in trimPrefixes is removed from the normalized value when the
// value does not contain a path list separator.
// The keep function is used to filter environment variables. If keep returns true, the
// variable is included in the result.
func NormalizeEnvVars(osVars []string, substitutions map[string]string, trimPrefixes []string, keep func(string) bool) map[string]string {
	result := make(map[string]string, len(osVars))
	var replacer *strings.Replacer
	if len(substitutions) > 0 {
		keys := make([]string, 0, len(substitutions))
		for k := range substitutions {
			if k != "" {
				keys = append(keys, k)
			}
		}
		slices.SortFunc(keys, func(a, b string) int {
			if len(a) != len(b) {
				return len(b) - len(a)
			}
			return strings.Compare(a, b)
		})
		oldnew := make([]string, 0, len(keys)*2)
		for _, k := range keys {
			oldnew = append(oldnew, k, substitutions[k])
		}
		if len(oldnew) > 0 {
			replacer = strings.NewReplacer(oldnew...)
		}
	}
	for _, kv := range osVars {
		ss := strings.SplitN(kv, "=", 2)
		if len(ss) < 2 {
			continue
		}
		key, value := ss[0], ss[1]
		if keep != nil && !keep(key) {
			continue
		}
		if replacer != nil {
			value = replacer.Replace(value)
		}
		// Do not trim path list prefixes (such as $PATH), where entries must remain
		// distinguishable from relative paths.
		if len(trimPrefixes) > 0 && !strings.Contains(value, string(os.PathListSeparator)) {
			for _, p := range trimPrefixes {
				if after, ok := strings.CutPrefix(value, p); ok {
					value = after
					break
				}
			}
		}
		result[key] = value
	}
	return result
}

// EnvVars returns the sorted, deduplicated, normalized RBE Command_EnvironmentVariable list for the cmd.
func (c *Cmd) EnvVars() []*rpb.Command_EnvironmentVariable {
	if len(c.Env) == 0 {
		return nil
	}
	envMap := NormalizeEnvVars(c.Env, c.EnvCacheKeySubstitutions, c.EnvCacheKeyTrimPrefixes, func(key string) bool {
		return !c.EnvCacheKeyOmits[key]
	})
	if len(envMap) == 0 {
		return nil
	}
	vars := make([]*rpb.Command_EnvironmentVariable, 0, len(envMap))
	for k, v := range envMap {
		vars = append(vars, &rpb.Command_EnvironmentVariable{
			Name:  k,
			Value: v,
		})
	}
	sort.Slice(vars, func(i, j int) bool {
		return vars[i].Name < vars[j].Name
	})
	return vars
}

// SetActionResult sets action result to the cmd.
func (c *Cmd) SetActionResult(result *rpb.ActionResult, cached bool) {
	c.actionResult = result
	c.cachedResult = cached
	// A new result must be flattened afresh: without this, a cmd whose cache
	// probe expanded a result then fell back to execution would record the
	// fresh result's directory outputs as empty.
	c.dirOutputsExpanded = false
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

// IsAuxiliary checks if the name is an auxiliary output.
// name and AuxiliaryLogOutputFiles/Dirs are workspace relative paths.
func (c *Cmd) IsAuxiliary(name path.Path) bool {
	if slices.Contains(c.AuxiliaryLogOutputFiles, name) {
		return true
	}
	if slices.Contains(c.AuxiliaryLogOutputDirs, name) {
		return true
	}
	return false
}

// isOutputFile reports whether fname is a declared output or under a declared
// output directory (c.outfiles holds both from InitOutputs). fname must use
// forward slashes: Path.Dir (not filepath.Dir) keeps the lookup keys
// slash-formed on Windows.
func (c *Cmd) isOutputFile(fname path.Path) bool {
	if c.outfiles[fname] {
		return true
	}
	for dir := fname.Dir(); dir != "." && dir != "/" && dir != ""; dir = dir.Dir() {
		if c.outfiles[dir] {
			return true
		}
	}
	return false
}

// entriesFromResult returns output file entries and additional entries for the cmd and result.
// output file entries will be recorded with cmdhash.
// additional output file entries will be recorded without cmdhash.
func (c *Cmd) entriesFromResult(ctx context.Context, ds hashfs.DataSource, updatedTime time.Time) (entries, additionalEntries []hashfs.UpdateEntry) {
	for _, f := range c.actionResult.GetOutputFiles() {
		if f.Digest == nil {
			continue
		}
		pname := c.WorkDir.Join(f.Path)
		if c.IsAuxiliary(pname) && !c.isOutputFile(pname) {
			continue
		}

		d := digest.FromProto(f.Digest)
		mode := fs.FileMode(0644)
		if f.IsExecutable {
			mode |= 0111
		}
		ent := hashfs.UpdateEntry{
			Name: pname,
			Entry: &merkletree.Entry{
				Name:         pname,
				Data:         blob.NewData(ds.Source(ctx, d, string(pname)), d),
				IsExecutable: f.IsExecutable,
			},
			Mode: mode,
			// ModTime must be the record time, never an mtime served by
			// the backend (e.g. REAPI NodeProperties.mtime): the cache-hit
			// path's self-healing for outputs removed from disk behind
			// siso's back relies on it moving forward. A same-digest
			// cache hit keeps the stale local-ready hashfs entry, and only
			// the mtime change makes Flush attempt a chtimes that trips
			// over the missing file, demoting the hit to a cache miss
			// whose remote re-execution forgets and re-records the
			// outputs. With an unchanged mtime, Flush would silently
			// succeed without re-materializing the file. If served mtimes
			// are ever honored here, the cache-hit path needs the same
			// ForgetOutputs-before-RecordOutputs as ProcessResult (then
			// cheap: with matching mtimes the flush is a single lstat via
			// matchesFileInfo). (see b/522434556)
			ModTime:     updatedTime,
			Action:      c.actionDigest,
			UpdatedTime: updatedTime,
			IsChanged:   true,
		}
		if !c.isOutputFile(pname) {
			// don't set cmdhash
			additionalEntries = append(additionalEntries, ent)
			continue
		}
		ent.CmdHash = c.CmdHash
		entries = append(entries, ent)
	}
	outputSymlinks := c.actionResult.GetOutputSymlinks()
	if len(outputSymlinks) == 0 && len(c.actionResult.GetOutputFileSymlinks())+len(c.actionResult.GetOutputDirectorySymlinks()) > 0 { //nolint:staticcheck // existing deprecation
		outputSymlinks = slices.Concat(
			c.actionResult.GetOutputFileSymlinks(),      //nolint:staticcheck // existing deprecation
			c.actionResult.GetOutputDirectorySymlinks(), //nolint:staticcheck // existing deprecation
		)
	}
	for _, s := range outputSymlinks {
		if s.Target == "" {
			continue
		}
		pname := c.WorkDir.Join(s.Path)
		if c.IsAuxiliary(pname) && !c.isOutputFile(pname) {
			continue
		}

		mode := fs.FileMode(0644) | fs.ModeSymlink
		entries = append(entries, hashfs.UpdateEntry{
			Name: pname,
			Entry: &merkletree.Entry{
				Name:   pname,
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
	for _, dd := range c.actionResult.GetOutputDirectories() {
		// It just needs to add the directories here because it assumes that they have already been expanded by ninja State.
		pname := c.WorkDir.Join(dd.Path)
		if c.IsAuxiliary(pname) && !c.isOutputFile(pname) {
			continue
		}

		mode := fs.FileMode(0755) | fs.ModeDir
		entries = append(entries, hashfs.UpdateEntry{
			Name: pname,
			Entry: &merkletree.Entry{
				Name: pname,
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
	c.preOutputEntries = c.HashFS.RetrieveUpdateEntries(ctx, c.WorkspaceRoot, c.AllOutputs())
}

// expandDirOutputs flattens each directory output in the action result in
// place, fetching its RBE Tree and replacing the bare directory node with the
// files, symlinks, and subdir nodes it contains, so entriesFromResult records
// its contents. A fetch/parse failure is returned (not swallowed) so the step
// fails loudly rather than recording an empty directory.
func (c *Cmd) expandDirOutputs(ctx context.Context, ds hashfs.DataSource) error {
	if c.actionResult == nil || len(c.actionResult.GetOutputDirectories()) == 0 {
		return nil
	}
	if c.dirOutputsExpanded {
		return nil
	}
	c.dirOutputsExpanded = true
	files := c.actionResult.GetOutputFiles()
	symlinks := c.actionResult.GetOutputSymlinks()
	if len(symlinks) == 0 && len(c.actionResult.GetOutputFileSymlinks())+len(c.actionResult.GetOutputDirectorySymlinks()) > 0 { //nolint:staticcheck // existing deprecation
		symlinks = slices.Concat(
			c.actionResult.GetOutputFileSymlinks(),      //nolint:staticcheck // existing deprecation
			c.actionResult.GetOutputDirectorySymlinks(), //nolint:staticcheck // existing deprecation
		)
	}
	var dirs []*rpb.OutputDirectory
	for _, d := range c.actionResult.GetOutputDirectories() {
		dname := c.WorkDir.Join(d.GetPath())
		// Auxiliary-only directories are kept for their Tree digest but are
		// not build graph outputs, so don't expand them. One that is also a
		// declared output is a real output: expand it.
		if c.IsAuxiliary(dname) && !c.isOutputFile(dname) {
			dirs = append(dirs, d)
			continue
		}
		treeDigest := digest.FromProto(d.GetTreeDigest())
		b, err := blob.DataToBytes(ctx, blob.NewData(ds.Source(ctx, treeDigest, d.GetPath()), treeDigest))
		if err != nil {
			return fmt.Errorf("fetch tree for dir output %s %s: %w", d.GetPath(), treeDigest, err)
		}
		store := blob.NewStore()
		root, err := reapi.ParseTree(ctx, c.HashFS.DigestFunction(), b, store)
		if err != nil {
			return fmt.Errorf("parse tree for dir output %s %s: %w", d.GetPath(), treeDigest, err)
		}
		dfiles, dsymlinks, ddirs := merkletree.Traverse(ctx, c.HashFS.DigestFunction(), d.GetPath(), root, store)
		// merkletree.Traverse joins with the OS separator; REAPI output paths
		// (and the hashfs keys they feed) are always forward-slash, so
		// normalize for Windows.
		for _, f := range dfiles {
			f.Path = filepath.ToSlash(f.Path)
		}
		for _, s := range dsymlinks {
			s.Path = filepath.ToSlash(s.Path)
		}
		for _, dd := range ddirs {
			dd.Path = filepath.ToSlash(dd.Path)
		}
		files = append(files, dfiles...)
		symlinks = append(symlinks, dsymlinks...)
		// Keep the root node and the subdir nodes so entriesFromResult
		// records a cmdhash entry for the directory and each subdirectory.
		dirs = append(dirs, d)
		dirs = append(dirs, ddirs...)
	}
	c.actionResult.OutputFiles = files
	c.actionResult.OutputSymlinks = symlinks
	c.actionResult.OutputDirectories = dirs
	return nil
}

// RecordOutputs records cmd's outputs from action result in hashfs.
func (c *Cmd) RecordOutputs(ctx context.Context, ds hashfs.DataSource, now time.Time) error {
	if err := c.expandDirOutputs(ctx, ds); err != nil {
		return fmt.Errorf("failed to expand directory outputs: %w", err)
	}
	// Replace, not merge: drop any previously recorded subtree for each
	// directory output so a member removed from the new tree does not linger.
	// HashFS.Update only upserts; the local path avoids stale members by
	// reading disk truth, so the remote/cache path needs this explicit prune.
	// ForgetOutputs (not Forget) because a directory output is owned by this
	// step: Forget is child-preserving and never evicts a directory node, so it
	// would leave the old members in place.
	c.HashFS.ForgetOutputs(ctx, c.WorkspaceRoot, c.OutputDirs)
	entries, additionalEntries := c.entriesFromResult(ctx, ds, now)
	clog.Infof(ctx, "output entries %d+%d", len(entries), len(additionalEntries))
	entries = c.computeOutputEntries(entries, now, c.CmdHash)
	if len(additionalEntries) > 0 {
		additionalEntries = c.computeOutputEntries(additionalEntries, now, nil)
		entries = append(entries, additionalEntries...)
	}
	err := c.HashFS.Update(ctx, c.WorkspaceRoot, entries)
	if err != nil {
		return fmt.Errorf("failed to update hashfs from remote: %w", err)
	}
	return nil
}

// RecordAuxiliaryOutputDigests computes and records auxiliary logs.
func (c *Cmd) RecordAuxiliaryOutputDigests(ctx context.Context, result *rpb.ActionResult) {
	if len(c.AuxiliaryLogOutputFiles) == 0 && len(c.AuxiliaryLogOutputDirs) == 0 {
		return
	}
	if result == nil {
		return
	}
	if c.AuxiliaryOutputDigests == nil {
		c.AuxiliaryOutputDigests = make(map[string]digest.Digest)
	}

	for _, file := range result.OutputFiles {
		fname := c.WorkDir.Join(file.Path)
		if c.IsAuxiliary(fname) {
			c.AuxiliaryOutputDigests[string(fname)] = digest.FromProto(file.Digest)
		}
	}
	for _, dir := range result.OutputDirectories {
		dname := c.WorkDir.Join(dir.Path)
		if c.IsAuxiliary(dname) {
			c.AuxiliaryOutputDigests[string(dname)+"/"] = digest.FromProto(dir.TreeDigest)
		}
	}
}

// updateLocalOutputDir records a locally produced directory output's contents
// in hashfs, tagging every entry with the producing step's cmdhash. The tag is
// what makes the subtree survive a state reload as generated output: an
// untagged directory is dropped by initDir, and an untagged file is reconciled
// as a source rather than an output.
func updateLocalOutputDir(ctx context.Context, hfs *hashfs.HashFS, root string, dir path.Path, cmdhash []byte, outputs map[path.Path]hashfs.UpdateEntry) (err error) {
	started := time.Now()
	defer func() {
		if err != nil {
			clog.Warningf(ctx, "failed to update local output dir %q: %v", dir, err)
		} else {
			clog.Infof(ctx, "update local output dir %q: %s", dir, time.Since(started))
		}
	}()

	entriesFromLocalDir := func(dir path.Path) ([]hashfs.UpdateEntry, error) {
		dents, err := hfs.ReadDir(ctx, root, dir)
		if err != nil {
			return nil, err
		}
		names := make([]path.Path, 0, len(dents))
		for _, dent := range dents {
			fname := dir.Join(dent.Name())
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
	for i := range ents {
		ents[i].CmdHash = cmdhash
	}
	return hfs.Update(ctx, root, ents)
}

// computeOutputEntries computes output entries to have updatedTime and cmdhash.
// if c.Restat or c.ResetContent is true, it checks preOutputEntries recorded
// by RecordPreOutputs and don't update mtime/is_changed
// if entry is the same as before.
func (c *Cmd) computeOutputEntries(entries []hashfs.UpdateEntry, updatedTime time.Time, cmdhash []byte) []hashfs.UpdateEntry {
	pre := make(map[path.Path]hashfs.UpdateEntry)
	if c.Restat || c.RestatContent {
		// check with previous content recorded by
		// RecordPreOutputs before execution.
		for _, ent := range c.preOutputEntries {
			pre[ent.Name] = ent
		}
	}

	var output path.Path
	if len(c.Outputs) > 0 {
		output = c.Outputs[0]
	} else if len(c.OutputDirs) > 0 {
		// Dir-only output: attach the edge hash to the declared directory, not
		// to entries[0] (a flattened child); outputMtime reads the edge hash
		// from the declared output, and an empty one silently disables
		// edge-change detection.
		output = c.OutputDirs[0]
	}
	ret := make([]hashfs.UpdateEntry, 0, len(entries))
	// Set cmdhash, updatedTime.
	// also isChanged=true if entry has been changed.
	for i, ent := range entries {
		ent.CmdHash = cmdhash
		if i == 0 || ent.Name == output {
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
				ent.IsChanged = pent.Entry.Data.Digest() != ent.Entry.Data.Digest()
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
	if c.PostProc != nil {
		err := c.PostProc(ctx, OutermostPaths(c.AllOutputs()))
		if err != nil {
			return err
		}
	}
	for _, dir := range c.ReconcileOutputdirs {
		c.HashFS.ForgetMissingsInDir(ctx, c.WorkspaceRoot, dir)
	}
	// AllOutputs includes the depfile, which is never in c.outfiles.
	var additionalOutputs []path.Path
	for _, out := range c.AllOutputs() {
		if !c.outfiles[out] {
			additionalOutputs = append(additionalOutputs, out)
		}
	}
	var additionalEntries []hashfs.UpdateEntry
	if len(additionalOutputs) > 0 {
		slices.SortFunc(additionalOutputs, func(a, b path.Path) int {
			if a < b {
				return -1
			}
			if a > b {
				return 1
			}
			return 0
		})
		entries := c.HashFS.RetrieveUpdateEntriesFromLocal(ctx, c.WorkspaceRoot, additionalOutputs)
		additionalEntries = c.computeOutputEntries(entries, now, nil)
	}
	outs := make([]path.Path, 0, len(c.outfiles))
	for out := range c.outfiles {
		outs = append(outs, out)
	}
	slices.SortFunc(outs, func(a, b path.Path) int {
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	})
	entries := c.HashFS.RetrieveUpdateEntriesFromLocal(ctx, c.WorkspaceRoot, outs)
	entries = c.computeOutputEntries(entries, now, c.CmdHash)
	entries = append(entries, additionalEntries...)
	err := c.HashFS.Update(ctx, c.WorkspaceRoot, entries)
	if err != nil {
		return fmt.Errorf("failed to update hashfs from local: %w", err)
	}
	outputs := make(map[path.Path]hashfs.UpdateEntry)
	for _, ent := range entries {
		outputs[ent.Name] = ent
	}
	for _, ent := range entries {
		if !ent.Mode.IsDir() {
			continue
		}
		// If output is dir, forget non-outputs under dir and retrieve
		// from local disk.
		err := updateLocalOutputDir(ctx, c.HashFS, c.WorkspaceRoot, ent.Name, c.CmdHash, outputs)
		if err != nil {
			return fmt.Errorf("failed to update hashfs from local dir %q: %w", ent.Name, err)
		}
	}
	if c.Restat || c.RestatContent {
		pre := make(map[path.Path]hashfs.UpdateEntry)
		for _, ent := range c.preOutputEntries {
			pre[ent.Name] = ent
		}
		ents := c.HashFS.RetrieveUpdateEntries(ctx, c.WorkspaceRoot, outs)
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
			if c.RestatContent {
				// Locally written outputs use mtime for restat even
				// when restat_content is set (computeOutputEntries
				// checks Restat && IsLocal first). The output is
				// considered changed here, but would be considered
				// unchanged if the step had run remotely, so the
				// results diverge between local and remote execution.
				clog.Warningf(ctx, "restat_content: local output mtime updated but content unchanged %q %s %s->%s", ent.Name, d, pent.ModTime, ent.ModTime)
				continue
			}
			clog.Warningf(ctx, "restat: changed but not modified %q %s %s->%s", ent.Name, d, pent.ModTime, ent.ModTime)
		}
	}
	return nil
}

// ResultFromEntries updates result from entries (collected from workspace).
func ResultFromEntries(ctx context.Context, fn digest.Function, result *rpb.ActionResult, dir string, entries []merkletree.Entry) {
	for _, ent := range entries {
		name, err := filepath.Rel(dir, string(ent.Name))
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
				TreeDigest: fn.Empty().Proto(),
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
	return fmt.Sprintf("exit=%d%s", e.ExitCode, exitCodeExplain(e.ExitCode))
}

// SetOutputResult sets output result.
func (c *Cmd) SetOutputResult(msg string) {
	c.outputResult = msg
}

// OutputResult gets output result.
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
