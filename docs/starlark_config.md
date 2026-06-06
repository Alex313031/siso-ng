# siso starlark config

siso uses [starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md)
for configuration to apply to
[ninja build rules](https://ninja-build.org/manual.html#_writing_your_own_ninja_files).

`siso ninja` subcommand will load starlark file
specified by `-load` flag at startup time.
Default is `@config//main.star` (`//build/config/siso/main.star`).

The starlark script should provide an
[init function to register handlers and step configs](#Initialization).
In the starlark script, `load` can load from current directory,
[@builtin](#builtin) or [@config](#config).

Adding a top level `strict_config = True` variable will add extra
validity checks on the config.

The starlark script will be evaluated at
[initialization phase](#Initialization)
and [per-step phase](#per_step-config)

### Example

```
load("@builtin//json.star", "json")
load("@builtin//runtime.star", "runtime")
load("@builtin//struct.star", "module")

def _stamp(ctx, cmd):
  out = cmd.outputs[0]
  ctx.actions.write(out)
  ctx.actions.exit(exit_status = 0)

def init(ctx):
  print("runtime os:%s arch:%s num:%d " % (runtime.os, runtime.arch, runtime.num_cpu)

  step_config = {}
  step_config["platforms"] = {
    "default": {
        "OSFamily": "Linux",
        "container-image": "docker://gcr.io/chops-private-images-prod/rbe/siso-chromium/linux@sha256:d4fcda628ebcdb3dd79b166619c56da08d5d7bd43d1a7b1f69734904cc7a1bb2",
    },
  }
  input_deps = {
    "some/toolchain/clang": [
       "some/toolchain/libclang.so",
       "some/toolchain/extra-inputs",
    ],
  }
  step_config["input_deps"] = input_deps
  rules = [
     {
       "name": "cxx/compile",
       "action": "cxx",
       "inputs": [
         "some/toolchain/clang",
       ],
       "remote": True,
     },
     {
       "name": "stamp",
       "action": "stamp",
       "handler": "stamp",
     },
  ]
  step_config["rules"] = rules
  return module(
    "config",
    step_config = json.encode(step_config),
    filegroups = {
       # "base:headers" is set in input_deps by using glob.
       # and it is used as precomputed tree for include dir "base",
       # when command line uses the directory as include dir.
       # e.g.
       #  dir="out/Default" command="clang -I../../base"
       "base:headers": {
          "type": "glob",
          "includes": ["*.h"],
       },
    },
    handlers = {
      "stamp": _stamp,
    }
 )
```

## Initialization

Before starting build process, `siso` calls `init` function with `ctx`
to register handlers and step configs.

`ctx` provides

  * `actions` module
    * `metadata` update metadata.
  * `metadata` dict
    * `args.gn`: contents of args.gn
    * `build.manifest`: digest of ninja build manifest
  * `flags` dict
    * command line flags. key doesn't have prefix `-` of flag.
      "project" is set even if it is specified by SISO_PROJECT_ID.
      "is_terminal" is set to indicate terminal or not, which would change
      some default value of flags.
      `-C` uses "dir" as key.
      targets (non-flags) uses "target" as key.
  * `fs`: path is workspace relative.
    * `read`: read contents.
      * `fname`: filename
    * `is_dir` check path is dir.
      * `fname`: filename
    * `exists` check path exists.
      * `fname`: filename
    * `size` get file size.
      * `fname`: filename
    * `canonpath`: convert ninja dir relative to workspace relative
      * `fname`: filename

`print` will print a message to log file.
`fail` will abort the process.

`init` should return a module.

   * `filegroups` dict
     * `filegroups` is used to create `input_deps` at initialization phase
       by using glob. It is recorded in `.siso_filegroups` and reuse
       without running glob if build graph and config is not changed.
     * key: filegroup label (i.e. dir:name).
     * value: dict of a filegroup generator
       * "type" specifies filegroup generator type.
         * "glob":
           * glob match from the dir specified by the label.
           * "includes": path patterns to include the glob
           * "excludes": path patterns to exclude the glob
         * TODO(b/266759797): add other type, like "filelist".
   * `handlers` dict
     * key: handler name
     * value: handler function used in [per-step config](#per_step-config).
   * `step_config` json string
     [`StepConfig`](../build/ninjabuild/step_config.go). It will be stored in `.siso_config` in working directory.
     * `properties`
       * a dict for config properties. used for resultstore if enabled.
       * key: property key
       * value: property value
     * `platforms`
       * key: platform reference name. "default" is used by default.
       * value: a dict of [platform properties](https://developers.google.com/remote-build-execution/docs/remote-execution-properties)
     * `sandbox`
       * a dict for sandbox. key "backend" specifies sandbox backend.
       * `backend` = `nsjail`
         * `nsjail_path`: a path to nsjail binary.
         * `nsjail_workdir`: a top directory for nsjail work dir (on same device with `nsjail_outdir`)
         * `nsjail_outdir`: a top directory of output directory (to make it writable)
     * `input_deps`
       * key: input path, or label (label contains ':').
         If the key is a path, it will be included in the expanded inputs.
         If the key is a label, it won't be included in the expanded inputs.
        `:headers` label would be used for c++ scandeps for include dirs
         or sysroots.
         e.g. dir="out/Default" and
              command="../../third_party/llvm-build/Release+Asserts/bin/clang
                    -I../../base  ..."
              then,
                 "third_party/llvm-build/Release+Asserts:headers"
                 "base:headers"
              are used as precomputed tree in the remote exec inputs.
       * values: other files or labels needed for the key.
         `<target>:inputs` label would be expanded to inputs of `<target>`'s
         inputs, if `<target>:inputs` is not explicitly defined in
         `input_deps`.
     * `case_sensitive_inputs`
       * a list of filenames for case sensitive filesystem.
         if "a.txt" and "A.txt" are in this, and "a.txt" is
         used as input, "A.txt" will be added as input too.
     * `inputs_requiring_clang_scandeps`
        a list of filename that requires clang scandeps.
        deprecated: use `scandeps.inputs_requiring_clang` instead.
     * `clang_scandeps`
        clang scandeps mode.
        deprecated: use `scandeps.use_clang` instead.
     * `scandeps`:
        * `inputs_requiring_clang`:
        a list of filename that requires clang scandeps.
        * `use_clang`:
          * "": don't use clang scandeps
          * "unsupported-macro": use clang scandeps when unsupported macro detected.
          * "scandeps-err": use clang scandeps when builtin scandeps failed.
        * `step_inputs`: [path_filter](#path_filter) specify what inputs from the ninja graph are used in addition to tool_inputs, scandeps results.
     * `bad_deps`
       * key: output target known to have bad deps
       * value: annotation (usually bug link)
     * `executables`
       * (Windows only) a list of filenames for executables.
         This is used to send Linux executables from Windows machine.
         e.g. node binary for typescript action.
     * `rules` list of `StepRule`.
        path is workspace relative, or ninja dir relative if it starts with "./"
        * identifier
          * `name`: unique name of the rule. required.
        * rule selector
          * `action`: regexp of action name. i.e. ninja rule name.
          * `action_outs`: output of the action.
          * `command_prefix`: prefix of step's command.
        * rule to apply
          * `inputs`: additional inputs
          * `exclude_input_patterns`: glob pattern to exclude from inputs
            * if it contains '/', full match to input path
            * otherwise, match basename of input path.
          * `indirect_inputs`: [path_filter](#path_filter) specify what
            inputs from the previous steps to use as inputs of this step.
            The matched indirect input are included recursively.
            Sibling outputs are also included. e.g. gen/{foo.stamp, foo.h, foo.cc} -> a step depending on gen/foo.stamp may need foo.h/foo.cc.
            Consider using `replace` or `accumulate` first, and use `indirect_inputs` only when they are not sufficient.
          * `outputs`: additional outputs. note: ignored in `cleandead`.
          * `outputs_map`: different deps based on outputs[0]
             * key: outputs[0]
             * value
               * `inputs`: additional inputs
               * `outputs`: additional outputs. note: ignored in `cleandead`.
               * `platform`: additional platform properties
               * `platform_ref`: overrides reference to platform properties
           * `auxiliary_log_output_files`: additional output files, that siso explicitly logs digest of.
             e.g. crash reports when RBE is used.
             siso logs the digest of the file for debugging (console or `siso_output` file), but doesn't download the file.
           * `auxiliary_log_output_dirs`: additional output dirs, that siso explicitly logs digest of.
             e.g. crash reports when RBE is used.
             siso logs the digest of the directory for debugging (console or `siso_output` file), but doesn't download files in the directory.
          * `restat`: true if step cmd reads output file and not write it
            (when no update needed), and considers output is clean if
             mtime is not changed (same as ninja's restat).
          * `restat_content`: true if step cmd considers output is clean if
             content is not changed (like ninja's restat, but not use mtime).
          * `platform_ref`: reference to platform properties
          * `platform`: additional platform properties
          * `remote`: use remote exec or not
          * `remote_wrapper`: a wrapper command used in remote execution
          * `remote_command`: args[0] will be replaced with remote_command.
          * `remote_inputs`
             * key: path of input of remote action
             * value: path of file (local) for the key path.
          * `input_root_absolute_path`: need `InputRootAbsolutePath` or not.
          * `canonicalize_dir`: ok to canonicalize work dir or not.
             enable by default, but disable if input_root_absolute_path is set.
          * `use_system_input`: ok to use input outside of workspace
             as it assumes those are platform container image.
          * `use_remote_exec_wrapper`: true if gomacc/rewrapper is used,
             so it runs locally without using deps/file trace.
          * `reproxy_config`: [`REProxyConfig`](../execute/cmd.go) if reproxy is used.
             the following RBE environment variables override the flags specified in the config:
             `RBE_exec_strategy`, `RBE_server_address`.
             See also https://github.com/bazelbuild/reclient/blob/main/docs/cmd-line-flags.md#rewrapper for more details.
          * `timeout`: duration of the step remote execution.
             See also `Timeout` field on [StepRule](../build/ninjabuild/step_config.go).
          * `exec_timeout`: duration of the action timeout of the step remote execution.
             See also `ExecTimeout` field on [StepRule](../build/ninjabuild/step_config.go).
          * `handler`: handler name to use for the step
          * `deps`: deps overrides
             * `gcc`: use `gcc -M`
             * `msvc`: use `clang-cl /showIncludes`
             * `depfile`: depfile variable of the step
             * `none`: ignore deps variable in ninja
          * `no_fast_deps`: disable fast-deps.
          * `output_local`: force download/outputs to local disk
          * `ignore_extra_input_pattern`: regexp to allow if it is used,
             but not listed in inputs.
          * `ignore_extra_output_pattern`: regexp to allow if it is generated,
             but not listed in outputs.
          * `impure`: mark it as not pure. i.e. not check inputs/outputs
          * `replace`: if any output of this step is used in other steps,
             those steps will use the inputs of this step as inputs
             instead of the outputs of this step.
             used for `stamp` step or so.
          * `accumulate`: if any output of this step is used in other steps,
             those steps will use the inputs and outputs of this step as inputs.
             used for thin archive or so.
             Not recursively accumulated.
          * `debug`: enable debug log in this step.

### path_filter

path_filter is used for `scandeps.step_inputs` and `indirect_inputs`.

 * `excludes`: A list of [glob patterns](#glob-pattern).
   Any input path that matches a pattern
   in this list is excluded. `excludes` are processed before `includes`.

 * `includes`: An optional list of [glob patterns](#glob-pattern) that
   specifies which inputs to include.

If `includes` is not specified or is empty, all inputs not excluded by
`excludes` are included.

If `includes` is specified, an input is included only if it matches a pattern
in`includes` (and is not excluded by `excludes`).

### glob pattern

 * If a pattern contains `/`, it is matched against the full path of an input.
 * Otherwise, the pattern is matched against the basename of the input's path.
 * The matching logic is equivalent to Go's [path.Match](https://pkg.go.dev/path#Match).

## per-step config

for each step, siso will apply the first matched rule.
If the rule has `handler`, it runs handler with `ctx`
and `cmd` when siso decides to run the step.
All inputs should be ready to use (via `ctx.fs`).

`ctx` provides

  * `actions`
    * `fix`: fix step
      * `inputs`: input pathnames
      * `tool_inputs`: input pathnames (not modified by deps)
        deprecated: use scandeps.step_inputs to filter step inputs with deps.
      * `outputs`: output pathnames. note: ignored in `cleandead`.
      * `args`: args for the step
      * `rspfile_content`: rspfile_content for the step.
      * `reproxy_config`: [`REProxyConfig`](../execute/cmd.go) in json-encoded format.
      * `reconcile_outputdirs`: reconcile directories to detect file removals in dirs after local step executions.
    * `write`: write file
      * `fname`: filename
      * `content`: content
      * `is_executable`: is_executable flag
    * `copy`: copy file
      * `src`: src filename
      * `dst`: dst filename
      * `recursive`: recursive copy?
    * `symlink`: create symlink
      * `target`: symlnk target
      * `linkpath`: symlink path.
    * `exit`: exit. don't run locally nor remotely.
      * `exit_status`: exit status
      * `stdout`: stdout
      * `stderr`: stderr
  * `fs`: same as `fs` in `ctx.fs` used in `init`.

`cmd` provides

  * `args` tuple: command line args
  * `envs` dict: environment variables
  * `dir` string: working directory
  * `workspace_root` string: absolute path to workspace
  * `deps` string: deps type
  * `inputs` list: input pathnames
  * `tool_inputs` list: input pathnames (rule's inputs).
    deprecated: use scandeps.step_inputs to filter inputs with deps.
  * `expanded_inputs`: func returns list: expanded input pathnames
  * `rspfile_content`: bytes: rspfile content
  * `outputs` list: output pathnames

`print` will print in log.

## @builtin

`@builtin` provides builtin modules/functions.

### [@builtin//encoding.star](../build/buildconfig/encoding.star)
provide [`json`](https://pkg.go.dev/go.starlark.net/lib/json)

### [@builtin//lib/gn.star](../build/buildconfig/lib/gn.star)
provide `gn`.

note: deprecated. parsing args.gn doesn't work well.
emit computed value and parse it, like gn_logs.txt.
https://crbug.com/427333789

 * `args`: returns a dict for content of args.gn.
   value is not interpreted, but just as string.
   e.g.
   * `foo=true` -> `gn.args(ctx)["foo"]=="true"`
   * `foo="data"` -> `gn.args(ctx)["foo"]=="\"data\""`

### [@builtin//path.star](../build/buildconfig/path.star)
provide `path`.

 * `base`: returns base name.
   * fname: filename
 * `dir`: return dir name.
   * fname: filename
 * `join`: join path elements.
   * args: path elements
 * `rel`: return relative path.
   * basepath: base path
   * targetpath: target path
 * `isabs`: check path is abs.
   * fname: filename

### [@builtin//runtime.star](../build/buildconfig/runtime.star)
provide `runtime`.

 * `num_cpu` int: number of cpus
 * `os` string: `GOOS` string.
 * `arch` string: `GOARCH` string

### [@builtin//struct.star](../build/buildconfig/struct.star)
provide [`struct`](https://pkg.go.dev/go.starlark.net/starlarkstruct#Struct)
and [`module`](https://pkg.go.dev/go.starlark.net/starlarkstruct#Module)

### @config

`@config` points `//build/config/siso` by default (changed by `--config`).
In chromium, `@config` is [//build/config/siso](https://chromium.googlesource.com/chromium/src/+/refs/heads/main/build/config/siso/)

### @config_overrides

`@config_overrides` provides access to local starlark files,
in `$workspace/.siso_remote`.
It is expected to have a module with name (basename of starlark) that
has `rules`, `input_deps` functions.
If file doesn't exist, it provides a None for the name (basename of starlark).

## References

* [starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md)
* [ninja build rules](https://ninja-build.org/manual.html#_writing_your_own_ninja_files)
