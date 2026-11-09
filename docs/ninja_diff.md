# Siso Key Differences from Ninja

## Remote execution capabilities

  * **Ninja:** Primarily focuses on local build execution. Ninja supports mainly
      c/cxx remote compiles by integrating with Reclient.
  * **Siso:** Offers extensive support for various remote execution.
      You can find Chromium's remote configurations [here](https://source.chromium.org/chromium/chromium/src/+/main:build/config/siso/).
      The configurations are currently maintained by the Browser Build team.

## Handling of missing output files during restat

  * **Ninja:** May silently ignore missing output files when performing `restat`.
  * **Siso:** Enforces stricter checks. If an output file is missing during `restat`, Siso will fail the build.

## Handling of missing input files during scheduling

  * **Ninja:** May silently ignore missing input files when scheduling build steps.
  * **Siso:** Enforces strict input dependency checks. If an input file is missing when scheduling a build step, Siso will fail the build.

## Targets from special syntax `target^`

  * **Ninja:** Specifies only the first target that contains the source file
      with [target^](https://ninja-build.org/manual.html#_running_ninja:~:text=a%20special%20syntax-,target%5E,-for%20specifying%20a).
      For example, `foo.c^` will specify `foo.o`.
  * **Siso:** Expands `target^` to all the targets that contains the source
      file. For example, `foo.java^` will get expanded to all Java steps that
      use files such as javac, errorprone, turbine etc.
  * **Siso:** For a header file, tries to find one of the source files include the
      header directly. For example, `foo.h^` will be treated as `foo.cc^`.
  * Requested in [crbug.com/396522989](https://crbug.com/396522989)

## Ninja file syntax changes

### Supports variables to configure remote execution

`remote_enabled`, `remote_platform_ref`, and `remote_timeout` can be used to control how the action
is run remotely. They can be specified on either a ninja `rule` or `build`. They have the same effect
as the fields with similar names in the StepConfig.

Note that upstream ninja will error out on seeing these extra bindings. But the android fork of
ninja has an exception to ignore them.

### output_local

`output_local = true` can be used to force writing all outputs of the action to local disk,
regardless of `-output_local_strategy`. `output_local = false` disables `output_local` set in the
StepConfig. It can be specified on either a ninja `rule` or `build`, and has the same effect as the
`output_local` field in the StepConfig.

It is applied when the action matches a StepConfig rule, or is configured with `remote_enabled`,
`remote_platform_ref` or `remote_timeout`. Other actions run locally and their outputs are
written to local disk anyway.

### restat_content

`restat_content` would work like `restat` but uses content digest instead of
mtime. i.e. step cmd considers output is clean if content is not changed.
It would be useful for restat=true with remote execution.
It could not be used for step that produces the same content but
update mtime to make it dirty.
`restat_content = false` or `restat_content = 0` can be used to disable `restat_content`.

With `SISO_EXPERIMENTS=auto-restat-content`, `restat = true` is treated as
`restat_content = true`, unless `restat_content` is set explicitly (in the
Ninja rule/build or the step config).
Outputs written locally, including local execution, local fallback and racing
results, still use mtime for `restat`, so this only affects outputs produced
by remote execution.

### sandbox_disabled

`sandbox_disabled = true` can be used to disable sandboxing for a specific rule in an otherwise
action-sandbox-enabled build. (see StepConfig.sandbox)

Note that upstream ninja will error out on seeing this extra binding. But the android fork of
ninja also implements sandboxing and this sandbox_disabled binding.

### cache_disabled

`cache_disabled = true` can be used to disable cache for a specific action.

Note that upstream ninja will error out on seeing this extra binding.

### Supports `phony_output` rule variable

  * **Ninja:** Doesn't have `phony_output`. But, Android's forked Ninja has a patch for the rule variable. See also [here](https://android.googlesource.com/platform/external/ninja/+/2ddc376cc3c5531db80899ce757861fac7a531b9/doc/manual.asciidoc#819)
  * **Siso:** Supports the variable for Android builds.

### Directory inputs and outputs

A `build` target ending in `/` (e.g. `gen/`) denotes a directory: an action can
declare a whole tree as an input or output instead of listing every file. Only
one step may produce a directory tree, and consumers depend on the whole tree;
Siso rejects an output produced under another step's directory output.

## Concurrent builds for the same build directory

  * **Ninja:** Allows multiple build invocations to run for the same build
      directory. There can be a race problem.
  * **Siso:** Locks the build directory so that other build invocations would
      wait until the current invocation to finish.

## Handling of depfile parse error

  * **Ninja:** Ignores a depfile parse error.
  * **Siso:** Fails for a depfile parse error.

## Re-run when inputs/outputs list has changed

 * **Ninja:** re-run when command line changed or inputs is newer than
     outputs.
 * **Siso:** similar with [n2](https://neugierig.org/software/blog/2022/03/n2.html),
     re-run when inputs/outputs list has changed too.

## Absolute paths in include/subninja statements

  * **Ninja:** Allows absolute paths in `include` and `subninja` statements.
  * **Siso:** Rejects absolute paths in `include` and `subninja` statements.
      Use relative paths instead.

## Path normalization

  * **Ninja:** Normalizes target paths when parsing `.ninja` files (e.g.
      collapsing redundant `/`, `.`, and `..` path segments).
  * **Siso:** Only strips a leading `./` and does not normalize target paths
      when parsing `.ninja` files to keep manifest parsing fast at scale (e.g.
      Android builds with >10GB `.ninja` manifests). Siso expects the build
      generator to emit normalized paths. See
      [crbug.com/578639457](https://crbug.com/578639457).

## Unsupported features

   Siso mainly intends to support `gn` (Chromium) and `soong` (Android).
   Support for other generators such as `cmake` is optional (see
   [crbug.com/448631316](https://crbug.com/448631316)).

   Siso may not support Ninja features if they are not used for Chromium or
   Android builds, e.g.
   [dynamic dependencies](https://ninja-build.org/manual.html#ref_dyndep),
   `ninja -t browse` etc.

