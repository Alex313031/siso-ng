// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

// StepDeps is a dependency of a step.
type StepDeps struct {
	Inputs      []string          `json:"inputs,omitempty"`
	Outputs     []string          `json:"outputs,omitempty"`
	Platform    map[string]string `json:"platform,omitempty"`
	PlatformRef string            `json:"platform_ref,omitempty"`
}

// StepRule is a rule for step.
type StepRule struct {
	// path is workspace relative
	// if path starts with ./, it is output dir relative.

	// Name is a step rule label. required.
	// must be unique to identify the step rule to make it easy
	// to maintain rules.
	Name string `json:"name"`

	// key

	// ActionName is a regexp to match with rule name.
	ActionName string         `json:"action,omitempty"`
	actionRE   *regexp.Regexp `json:"-"`
	// ActionOuts matches  with outputs of the step.
	ActionOuts []string `json:"action_outs,omitempty"`

	// CommandPrefix matches the command prefix of the step.
	// If argv[0] is absolute path outside of the workspace,
	// it is compared with basename of argv[0].
	// Note: it doesn't support space in argv[0] for such case.
	CommandPrefix string `json:"command_prefix,omitempty"`

	// rule to apply

	// Inputs are inputs to add to the step.
	Inputs []string `json:"inputs,omitempty"`

	// ExcludeInputPatterns are path glob patterns to exclude from the expanded inputs.
	ExcludeInputPatterns []string `json:"exclude_input_patterns,omitempty"`

	// IndirectInputs enables indirect (transitive, recursive) inputs
	// as action input of the step.
	IndirectInputs *PathFilter `json:"indirect_inputs,omitempty"`

	// Outputs are outputs to add to the step.
	Outputs []string `json:"outputs,omitempty"`
	// OutputsMap is a map to fix inputs/outputs per outputs.
	OutputsMap map[string]StepDeps `json:"outputs_map,omitempty"`

	// AuxiliaryLogOutputFiles are output files that siso explicitly logs digest of
	// but doesn't download to disk.
	AuxiliaryLogOutputFiles []string `json:"auxiliary_log_output_files,omitempty"`

	// AuxiliaryLogOutputDirs are output directories that siso explicitly logs digest of
	// but doesn't download to disk.
	AuxiliaryLogOutputDirs []string `json:"auxiliary_log_output_dirs,omitempty"`

	// Restat means the step command will read its output
	// and may not write output when no update needed.
	// Output will be considered as clean if output mtime
	// is not changed by the command execution.
	// https://ninja-build.org/manual.html#ref_rule:~:text=appears%20in%20commands.-,restat,-if%20present%2C%20causes
	Restat bool `json:"restat,omitempty"`

	// RestatContent means output will be considered as clean
	// if output content is the same as before.
	RestatContent bool `json:"restat_content,omitempty"`

	// PlatformRef is reference to platform properties.
	PlatformRef string `json:"platform_ref,omitempty"`
	// Platform is platform properties.
	// TODO: siso: prefix will not send to remote backend.
	Platform map[string]string `json:"platform,omitempty"`

	// Remote marks the step is remote executable.
	Remote bool `json:"remote,omitempty"`
	// RemoteWrapper is a wrapper used in remote execution.
	// TODO: put RemoteWrapper in Platform["siso:remote_wrapper"]
	RemoteWrapper string `json:"remote_wrapper,omitempty"`
	// RemoteCommand is a command used in the first argument.
	RemoteCommand string `json:"remote_command,omitempty"`
	// RemoteInputs is a map for remote execution.
	// path in remote action -> local path
	RemoteInputs map[string]string `json:"remote_inputs,omitempty"`
	// InputRootAbsolutePath indicates the step requires absolute path for the input root, i.e. not relocatable.
	InputRootAbsolutePath bool `json:"input_root_absolute_path,omitempty"`
	// CanonicalizeDir indicates the step can canonicalize the working dir.
	// true, false or not-set(when nil), and treated as true when not set.
	CanonicalizeDir *bool `json:"canonicalize_dir,omitempty"`

	// UseSystemInput indicates to allow extra inputs outside of workspace.
	UseSystemInput bool `json:"use_system_input,omitempty"`

	// UseRemoteExecWrapper indicates the command uses remote exec wrapper
	// (e.g. gomacc, rewrapper), so
	// - no need to run `clang -M`
	// - run locally but more parallelism
	// - no file access trace
	UseRemoteExecWrapper bool `json:"use_remote_exec_wrapper,omitempty"`
	// REProxyConfig specifies configuration options for using reproxy.
	REProxyConfig *execute.REProxyConfig `json:"reproxy_config,omitempty"`

	// Timeout specifies time duration for the remote execution call of the step.
	// This covers the remote execution overheads that are not covered by
	// the action timeout. e.g. scheduling, pre/post execution steps, network
	// overheads etc.
	Timeout string `json:"timeout,omitempty"` // duration format

	// ExecTimeout specifies time duration for the action's timeout for the remote execution call
	// of the step.
	// If not specified, Timeout*2 will be set to expect cache hit for long command execution
	// in the next build.
	// See also the explanation of the action timeout in REAPI.
	// https://github.com/bazelbuild/remote-apis/blob/e94a7ece2a1e8da1dcf278a0baf2edfe7baafb94/build/bazel/remote/execution/v2/remote_execution.proto#L610-L634
	ExecTimeout string `json:"exec_timeout,omitempty"` // duration format

	// Handler name.
	Handler string `json:"handler,omitempty"`

	// Deps specifies deps type.
	//
	//   deps="gcc": Use `gcc -M` or deps log.
	//   deps="msvc": Use `clang-cl /showIncludes` or deps log.
	//   deps="depfile": Use `depfile` if `depfile` is specified.
	//   deps="none": ignore deps of the step.
	Deps string `json:"deps,omitempty"`

	// OutputLocal indicates to force to write output files to local disk
	// for subsequent steps.
	// TODO: better to have `require_local_inputs`=[<globs>] to reduce unnecessary downloads?
	OutputLocal bool `json:"output_local,omitempty"`

	// IgnoreExtraInputPattern specifies regexp to ignore extra inputs.
	// ignore extra input detected by strace if it matches with this pattern
	// e.g. cache file
	IgnoreExtraInputPattern string `json:"ignore_extra_input_pattern,omitempty"`

	// IgnoreExtraOutputPattern specifies regexp to ignore extra outputs.
	// ignore extra output detected by strace if it matches with this pattern
	// e.g. cache file
	IgnoreExtraOutputPattern string `json:"ignore_extra_output_pattern,omitempty"`

	// Impure marks the step is impure, i.e. allow extra inputs/outputs.
	// Better to use above options if possible.
	// Impure disables file access trace.
	Impure bool `json:"impure,omitempty"`

	// Replace replaces the outputs, when used by other step,
	// to the inputs of the step.
	// e.g. stamp.
	Replace bool `json:"replace,omitempty"`

	// Accumulate accumulates the inputs of the step to
	// the outputs, when used by other step.
	// e.g. thin archive.
	Accumulate bool `json:"accumulate,omitempty"`

	// Debug indicates to log debug information for the step.
	Debug bool `json:"debug,omitempty"`
}

// Init initializes the step rule.
func (r *StepRule) Init() error {
	if r.ActionName != "" {
		var err error
		pat := r.ActionName
		if !strings.HasPrefix(pat, "^") {
			pat = "^" + pat
		}
		if !strings.HasSuffix(pat, "$") {
			pat = pat + "$"
		}
		r.actionRE, err = regexp.Compile(pat)
		if err != nil {
			return err
		}
	}
	if r.ActionName == "" && len(r.ActionOuts) == 0 && r.CommandPrefix == "" {
		buf, err := json.Marshal(r)
		return fmt.Errorf("no selector in rule %s: %w", buf, err)
	}
	sort.Strings(r.Inputs)
	return nil
}

// StepConfig is a config for ninja build manifest.
type StepConfig struct {
	StateDir string `json:"-"`

	// Properties are config properties.
	// Used for resultstore if enabled.
	Properties map[string]string `json:"properties,omitempty"`

	// Platforms specifies platform properties.
	Platforms map[string]map[string]string `json:"platforms,omitempty"`

	// InputDeps specifies additional input files for a input file.
	// If key contains ":", it is considered as label, and
	// label itself is removed from expanded input list, but
	// label's values are added to expanded input list.
	InputDeps map[string][]string `json:"input_deps,omitempty"`

	// CaseSensitiveInputs lists case sensitive input filenames.
	// use these case sensitive filename. apply only for deps?
	CaseSensitiveInputs []string `json:"case_sensitive_inputs,omitempty"`

	// Scandeps specifies scandeps config.
	Scandeps *ScandepsConfig `json:"scandeps,omitempty"`

	// InputsRequiringClangScandeps lists inputs that requires clang
	// scan deps.
	// deprecated: use scandeps.inputs_requiring_clang instead.
	InputsRequiringClangScandeps []string `json:"inputs_requiring_clang_scandeps,omitempty"`

	// ClangScandeps specifies clang scandeps mode.
	//  - "" - no clang scandeps
	//  - "unsupported-macro" - if unsupported macro is detected.
	//  - "scandeps-err" - if scandeps failed.
	// deprecated: use scandeps.use_clang instead.
	ClangScandeps string `json:"clang_scandeps,omitempty"`

	// Rules lists step rules.
	Rules []*StepRule `json:"rules,omitempty"`

	// BadDeps specifies known targets with bad deps,
	// i.e. target has other generated targets not in direct/indirect
	// dependencies in depfile.
	// This target won't cause error with bad deps even with
	// `SISO_EXPERIMENTS=fail-on-bad-deps` to make it easy to
	// detect new bad deps.
	// key is output target known to have bad deps.
	// value is annotation (usually bug link).
	BadDeps map[string]string `json:"bad_deps,omitempty"`

	// Executables are files that need to have executable bit on Linux worker.
	// This field is used to upload Linux executables from Windows host.
	Executables []string `json:"executables,omitempty"`

	// Sandbox is sandbox config
	Sandbox map[string]string `json:"sandbox,omitempty"`
}

// ScandepsConfig is a config for scandeps.
type ScandepsConfig struct {
	// InputsRequiringClang lists inputs that requires clang
	// scan deps.
	InputsRequiringClang []string `json:"inputs_requiring_clang,omitempty"`

	// UseClang specifies when to use `clang -M` for scandeps.
	//  - "" - no clang scandeps
	//  - "unsupported-macro" - if unsupported macro is detected.
	//  - "scandeps-err" - if scandeps failed.
	UseClang string `json:"use_clang,omitempty"`

	// StepInputs filters step inputs for as action inputs
	// in addition to scandeps results and tool_inputs.
	// If not set, all step inputs will be discarded and scandeps results
	// and tool_inputs are used.
	StepInputs       *PathFilter `json:"step_inputs,omitempty"`
	stepInputsFilter func(context.Context, string, bool) bool
}

// Init initializes StepConfig.
func (sc *StepConfig) Init(ctx context.Context) error {
	seen := make(map[string]bool)
	for _, rule := range sc.Rules {
		if rule == nil {
			return fmt.Errorf("encountered nil rule")
		}
		if rule.Name == "" {
			buf, err := json.Marshal(rule)
			return fmt.Errorf("no name in rule: %s: %w", buf, err)
		}
		if seen[rule.Name] {
			buf, err := json.Marshal(rule)
			return fmt.Errorf("duplicate name in rule %s: %w", buf, err)
		}
		seen[rule.Name] = true
		err := rule.Init()
		if err != nil {
			clog.Errorf(ctx, "Failed to init rule %q: %v", rule.Name, err)
			return fmt.Errorf("failed to init rule %q: %w", rule.Name, err)
		}
		if rule.PlatformRef != "" {
			if _, ok := sc.Platforms[rule.PlatformRef]; !ok {
				return fmt.Errorf("platform_ref %q in rule %q not found in platforms", rule.PlatformRef, rule.Name)
			}
		}
		for optName, opt := range rule.OutputsMap {
			if opt.PlatformRef != "" {
				if _, ok := sc.Platforms[opt.PlatformRef]; !ok {
					return fmt.Errorf("platform_ref %q in outputs_map %q of rule %q not found in platforms", opt.PlatformRef, optName, rule.Name)
				}
			}
		}
	}
	if sc.Scandeps == nil {
		sc.Scandeps = &ScandepsConfig{
			InputsRequiringClang: sc.InputsRequiringClangScandeps,
			UseClang:             sc.ClangScandeps,
		}
		sc.InputsRequiringClangScandeps = nil
		sc.ClangScandeps = ""
	}
	if len(sc.InputsRequiringClangScandeps) > 0 || sc.ClangScandeps != "" {
		return fmt.Errorf("inputs_requiring_clang_scandeps and clang_scandeps is deprecated. just use scandeps")
	}
	if sc.Scandeps.StepInputs.enabled() {
		sc.Scandeps.stepInputsFilter = sc.Scandeps.StepInputs.filter(ctx, "scandeps.step_inputs")
	}
	if sc.InputDeps == nil {
		sc.InputDeps = make(map[string][]string)
	}
	return nil
}

// UpdateFilegroups updates filegroups (input_deps) in the step config.
func (sc *StepConfig) UpdateFilegroups(ctx context.Context, filegroups map[string][]string) error {
	maps.Copy(sc.InputDeps, filegroups)
	return nil
}

func fromConfigPath(ctx context.Context, p *build.Path, path string) string {
	if strings.HasPrefix(path, "./") {
		return p.MaybeFromRelative(ctx, path)
	}
	return path
}

func toConfigPath(p *build.Path, path string) string {
	path = filepath.ToSlash(path)
	if after, ok := strings.CutPrefix(path, p.BaseDir+"/"); ok {
		return "./" + after
	}
	return path
}

// Lookup returns a step rule for the edge.
func (sc StepConfig) Lookup(ctx context.Context, bpath *build.Path, edge *ninjautil.Edge) (StepRule, bool) {
	var out, outConfig string
	if len(edge.Outputs()) > 0 {
		out = bpath.MaybeFromRelative(ctx, edge.Outputs()[0].Path())
		outConfig = toConfigPath(bpath, out)
	}
	actionName := edge.RuleName()
	command := edge.RawBinding("command")
	args0, args, ok := strings.Cut(command, " ")
	// python3.exe may be absolute path in depot_tools, but
	// config uses "python3.exe"...
	// TODO(ukai): use workspace relative if it is in workspace?
	if ok {
		args0 = strings.Trim(args0, `"`)
		args0 = strings.ReplaceAll(args0, "$:", ":")
		if filepath.IsAbs(args0) && !strings.HasPrefix(args0, bpath.WorkspaceRoot) {
			args0 = filepath.Base(args0)
			command = args0 + " " + args
			// TODO(b/277532415): preserve quote of args0?
		}
	}
	if log.V(1) {
		clog.Infof(ctx, "lookup action:%s out:%s args0:%s", actionName, out, args0)
	}

	remoteBinding := edge.Binding("remote_enabled")
	platformRefBinding := edge.Binding("remote_platform_ref")
	timeoutBinding := edge.Binding("remote_timeout")

loop:
	for _, c := range sc.Rules {
		if c.actionRE != nil {
			matched := c.actionRE.MatchString(actionName)
			if !matched {
				continue loop
			}
		}
		if len(c.ActionOuts) > 0 {
			match := slices.Contains(c.ActionOuts, outConfig)
			if !match {
				continue loop
			}
		}
		if c.CommandPrefix != "" {
			match := false
			if strings.HasPrefix(command, c.CommandPrefix) {
				match = true
			}
			// TODO(ukai): evaluate command if command prefix is longer than initial literal?
			if !match {
				continue loop
			}
		}

		rule := *c
		rule.actionRE = nil
		opt := rule.OutputsMap[outConfig]

		if remoteBinding != "" {
			rule.Remote = (remoteBinding == "true")
			if !rule.Remote {
				rule.Platform = nil
			}
		}
		if timeoutBinding != "" {
			rule.Timeout = timeoutBinding
		}

		if rule.Remote {
			if len(rule.Platform) == 0 {
				rule.Platform = make(map[string]string)
			}
			ref := "default"
			if platformRefBinding != "" {
				ref = platformRefBinding
			} else if opt.PlatformRef != "" {
				ref = opt.PlatformRef
			} else if rule.PlatformRef != "" {
				ref = rule.PlatformRef
			}
			p := sc.Platforms[ref]
			for k, v := range p {
				if _, ok := rule.Platform[k]; !ok {
					rule.Platform[k] = v
				}
			}
			if rule.InputRootAbsolutePath {
				rule.Platform["InputRootAbsolutePath"] = bpath.WorkspaceRoot
			}
		}

		if bool(log.V(1)) || rule.Debug {
			clog.Infof(ctx, "hit %s actionName:%q out:%q args0:%q -> action_name:%q action_outs:%q command:%q inputs:%d+%d outputs:%d+%d output-local:%t platform:%v + %v replace:%t accumulate:%t", rule.Name, actionName, outConfig, args0, rule.ActionName, rule.ActionOuts, rule.CommandPrefix, len(rule.Inputs), len(opt.Inputs), len(rule.Outputs), len(opt.Outputs), rule.OutputLocal, rule.Platform, opt.Platform, rule.Replace, rule.Accumulate)
		}

		inputs := make([]string, 0, len(rule.Inputs)+len(opt.Inputs))
		inputs = append(inputs, rule.Inputs...)
		inputs = append(inputs, opt.Inputs...)
		for i := range inputs {
			inputs[i] = fromConfigPath(ctx, bpath, inputs[i])
		}
		rule.Inputs = inputs
		if len(rule.RemoteInputs) > 0 {
			m := make(map[string]string)
			for k, v := range rule.RemoteInputs {
				k = fromConfigPath(ctx, bpath, k)
				v = fromConfigPath(ctx, bpath, v)
				m[k] = v
			}
			rule.RemoteInputs = m
		}
		outputs := make([]string, 0, len(rule.Outputs)+len(opt.Outputs))
		outputs = append(outputs, rule.Outputs...)
		outputs = append(outputs, opt.Outputs...)
		for i := range outputs {
			outputs[i] = fromConfigPath(ctx, bpath, outputs[i])
		}
		rule.Outputs = outputs

		for i := range rule.AuxiliaryLogOutputFiles {
			rule.AuxiliaryLogOutputFiles[i] = fromConfigPath(ctx, bpath, rule.AuxiliaryLogOutputFiles[i])
		}
		for i := range rule.AuxiliaryLogOutputDirs {
			rule.AuxiliaryLogOutputDirs[i] = fromConfigPath(ctx, bpath, rule.AuxiliaryLogOutputDirs[i])
		}

		if len(opt.Platform) > 0 {
			if len(rule.Platform) == 0 {
				rule.Platform = make(map[string]string)
			}
			maps.Copy(rule.Platform, opt.Platform)
		}
		return rule, !c.Impure
	}

	if remoteBinding != "" || platformRefBinding != "" || timeoutBinding != "" {
		clog.Infof(ctx, "miss, but configured in ninja: actionName:%q out:%q args0:%q", actionName, out, args0)
		rule := StepRule{
			Name: "ninja:" + actionName,
		}
		if remoteBinding != "" {
			rule.Remote = (remoteBinding == "true")
		}
		if timeoutBinding != "" {
			rule.Timeout = timeoutBinding
		}
		if rule.Remote {
			if len(rule.Platform) == 0 {
				rule.Platform = make(map[string]string)
			}
			ref := "default"
			if platformRefBinding != "" {
				ref = platformRefBinding
			}
			p := sc.Platforms[ref]
			for k, v := range p {
				if _, ok := rule.Platform[k]; !ok {
					rule.Platform[k] = v
				}
			}
		}
		if bool(log.V(1)) || rule.Debug {
			clog.Infof(ctx, "hit ninja properties %s actionName:%q out:%q args0:%q -> remote:%t timeout:%q platform:%v", rule.Name, actionName, outConfig, args0, rule.Remote, rule.Timeout, rule.Platform)
		}
		return rule, true
	}

	clog.Infof(ctx, "miss actionName:%q out:%q args0:%q", actionName, out, args0)
	return StepRule{}, false
}

type depPathPair struct{ dep, path string }

// for log missing input only once per path or depPathPair.
var knownMissingInputs sync.Map // {path or depPathPair} -> true

// ExpandInputs expands inputs, and returns paths separated by slash.
func (sc StepConfig) ExpandInputs(ctx context.Context, p *build.Path, hashFS *hashfs.HashFS, paths []string) []string {
	seen := make(map[string]bool)
	var expanded []string
	for i := 0; i < len(paths); i++ {
		path := paths[i]
		if seen[path] {
			continue
		}
		seen[path] = true
		if !strings.Contains(path, ":") {
			_, err := hashFS.Stat(ctx, p.WorkspaceRoot, path)
			if err != nil {
				if _, loaded := knownMissingInputs.LoadOrStore(path, true); !loaded {
					// TODO(b/271783311): hard error for bad config
					clog.Warningf(ctx, "missing inputs %s", path)
				}
			} else {
				expanded = append(expanded, filepath.ToSlash(path))
			}
		}
		path = toConfigPath(p, path)
		deps, ok := sc.InputDeps[path]
		if ok {
			if log.V(1) {
				clog.Infof(ctx, "input-deps expand %s", path)
			}
			for _, dep := range deps {
				dep := fromConfigPath(ctx, p, dep)
				if strings.Contains(dep, ":") {
					paths = append(paths, dep)
					continue
				}
				_, err := hashFS.Stat(ctx, p.WorkspaceRoot, dep)
				if err != nil {
					if _, loaded := knownMissingInputs.LoadOrStore(depPathPair{dep, path}, true); !loaded {
						clog.Warningf(ctx, "missing file in input-dep %s (from %s): %v", dep, path, err)
					}
					continue
				}
				paths = append(paths, dep)
			}
		}
	}
	sort.Strings(expanded)
	return expanded
}
