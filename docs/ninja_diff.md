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

## Supports `phony_output` rule variable

  * **Ninja:** Doesn't have `phony_output`. But, Android's forked Ninja has a patch for the rule variable. See also [here](https://android.googlesource.com/platform/external/ninja/+/2ddc376cc3c5531db80899ce757861fac7a531b9/doc/manual.asciidoc#819)
  * **Siso:** Supports the variable for Android builds.

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

## Unsupported features

   Siso may not support Ninja features if they are not used for Chromium
   builds. e.g. [dynamic dependencies](https://ninja-build.org/manual.html#ref_dyndep), `ninja -t browse` etc

