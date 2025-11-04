// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
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

// IndirectInputs specifies what indirect inputs are used as action inputs.
type IndirectInputs struct {
	// glob pattern to use as action inputs from indirect inputs.
	Includes []string `json:"includes,omitempty"`

	// add other options? max depth, exclude etc?
}

// enabled returns true when IndirectInputs is enabled.
func (ii *IndirectInputs) enabled() bool {
	if ii == nil {
		return false
	}
	return len(ii.Includes) > 0
}

func (ii *IndirectInputs) filter(ctx context.Context) func(context.Context, string, bool) bool {
	var m []func(context.Context, string, bool) bool
	for _, in := range ii.Includes {
		if in == "*" {
			// match any file
			m = append(m, func(ctx context.Context, p string, debug bool) bool {
				if debug {
					clog.Infof(ctx, "match any: %q", p)
				}
				return true
			})
			continue
		}
		if strings.HasPrefix(in, "*") && !strings.ContainsAny(in[1:], "*?[\\/") {
			// just has * prefix, and no pattern meta or '/' in suffix.
			// just suffix match with base name.
			suffix := in[1:]
			m = append(m, func(ctx context.Context, p string, debug bool) bool {
				ok := strings.HasSuffix(path.Base(p), suffix)
				if debug {
					clog.Infof(ctx, "match suffix %q: %q => %t", suffix, p, ok)
				}
				return ok
			})
			continue
		}
		// just check ErrBadPattern for pattern `in`.
		// it's sufficient to check error once, and no other way
		// to test pattern.
		_, err := path.Match(in, in)
		if err != nil {
			clog.Warningf(ctx, "bad indirect_inputs.includes pattern %q: %v", in, err)
			continue
		}
		pattern := in
		if strings.Count(in, "/") == 0 {
			// basename match.
			m = append(m, func(ctx context.Context, p string, debug bool) bool {
				b := path.Base(p)
				ok, _ := path.Match(pattern, b)
				if debug {
					clog.Infof(ctx, "match pattern(base) %q: %q => %t", pattern, p, ok)
				}
				return ok
			})
			continue
		}
		m = append(m, func(ctx context.Context, p string, debug bool) bool {
			ok, _ := path.Match(pattern, p)
			if debug {
				clog.Infof(ctx, "match pattern %q: %q => %t", pattern, p, ok)
			}
			return ok
		})
	}
	return func(ctx context.Context, p string, debug bool) bool {
		for _, f := range m {
			if f(ctx, p, debug) {
				return true
			}
		}
		if debug {
			clog.Infof(ctx, "match none %q", p)
		}
		return false
	}
}

// StepRule is a rule for step.
type StepRule struct {
	// path is exec root relative
	// if path starts with ./, it is working directory relative.

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

	// CommandPreifx matches with command prefix of the step.
	// If argv[0] is absolute path outside of execroot,
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
	IndirectInputs *IndirectInputs `json:"indirect_inputs,omitempty"`

	// Outputs are outputs to add to the step.
	Outputs []string `json:"outputs,omitempty"`
	// OutputsMap is a map to fix inputs/outputs per outputs.
	OutputsMap map[string]StepDeps `json:"outputs_map,omitempty"`

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
	CanonicalizeDir bool `json:"canonicalize_dir,omitempty"`

	// UseSystemInput indicates to allow extra inputs outside exec root.
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

	// NoFastDeps disables fast-deps.
	NoFastDeps bool `json:"no_fast_deps,omitempty"`

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

	// InputsRequiringClangScandeps lists inputs that requires clang
	// scan deps.
	InputsRequiringClangScandeps []string `json:"inputs_requiring_clang_scandeps,omitempty"`

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
}

// Init initializes StepConfig.
func (sc StepConfig) Init(ctx context.Context) error {
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
	}
	return nil
}

// UpdateFilegroups updates filegroups (input_deps) in the step config.
func (sc StepConfig) UpdateFilegroups(ctx context.Context, filegroups map[string][]string) error {
	maps.Copy(sc.InputDeps, filegroups)
	return nil
}

func fromConfigPath(ctx context.Context, p *build.Path, path string) string {
	if strings.HasPrefix(path, "./") {
		return p.MaybeFromWD(ctx, path)
	}
	return path
}

func toConfigPath(p *build.Path, path string) string {
	path = filepath.ToSlash(path)
	if after, ok := strings.CutPrefix(path, p.Dir+"/"); ok {
		return "./" + after
	}
	return path
}

// Lookup returns a step rule for the edge.
func (sc StepConfig) Lookup(ctx context.Context, bpath *build.Path, edge *ninjautil.Edge) (StepRule, bool) {
	var out, outConfig string
	if len(edge.Outputs()) > 0 {
		out = bpath.MaybeFromWD(ctx, edge.Outputs()[0].Path())
		outConfig = toConfigPath(bpath, out)
	}
	actionName := edge.RuleName()
	command := edge.RawBinding("command")
	args0, args, ok := strings.Cut(command, " ")
	// ptyhon3.exe may be absolute path in depot_tools, but
	// config uses "python3.exe"...
	// TODO(ukai): use execroot relative if it is in execroot?
	if ok {
		args0 = strings.Trim(args0, `"`)
		args0 = strings.ReplaceAll(args0, "$:", ":")
		if filepath.IsAbs(args0) && !strings.HasPrefix(args0, bpath.ExecRoot) {
			args0 = filepath.Base(args0)
			command = args0 + " " + args
			// TODO(b/277532415): preserve quote of args0?
		}
	}
	if log.V(1) {
		clog.Infof(ctx, "lookup action:%s out:%s args0:%s", actionName, out, args0)
	}

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
		if rule.Remote {
			if len(rule.Platform) == 0 {
				rule.Platform = make(map[string]string)
			}
			ref := "default"
			if opt.PlatformRef != "" {
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
				rule.Platform["InputRootAbsolutePath"] = bpath.ExecRoot
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

		if len(opt.Platform) > 0 {
			if len(rule.Platform) == 0 {
				rule.Platform = make(map[string]string)
			}
			maps.Copy(rule.Platform, opt.Platform)
		}
		return rule, !c.Impure
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
			_, err := hashFS.Stat(ctx, p.ExecRoot, path)
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
				_, err := hashFS.Stat(ctx, p.ExecRoot, dep)
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
