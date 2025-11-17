# Siso-ng

Siso-ng is a fork of [Chromium's Siso](https://chromium.googlesource.com/build/+/refs/heads/main/siso/README.md), 
with support for [Ninja's](https://ninja-build.org/) `-j` flag and logging enhancements.

Siso is a build tool that aims to significantly speed up Chromium's build.

* It is a drop-in replacement for Ninja, which means it can be easily used
  instead of Ninja without requiring a migration or change in developer's
  workflows.
* It runs build actions on RBE natively.
* It avoids stat, disk and network I/O as much as possible.
* It reduces CPU usage and memory consumption by sharing in one process memory
  space.
* It collects performance metrics for each action during a build and allows to
  analyze them using cloud trace/cloud profiler.

## Where did the name "Siso" come from?

Siso is named after shiso, a herb commonly used in Japan. It's a reference to basil and the Bazel build system. Siso is an alternative romanization of shiso and more typeable than shiso (but still pronounced shiso). Considering how often we type the name of a build tool every day, we decided to optimize for that. ;)

## Building

See [Building](./docs/development.md#build-the-code).

## Documents

- [Siso authentication options](./docs/auth.md) explains authentication
  options available in Siso to communicate with RE API backend.
- [Siso environment variables](./docs/environment_variables.md) explains
  environment variables used by Siso.
- [Siso starlark config](./docs/starlark_config.md) explains
  Siso configs (e.g. `//build/config/siso/main.star`).
- [REAPI platform properties](./docs/reapi_platform_properties.md) explains
  RE API platform properties used in Siso.
- [Key difference from Ninja](./docs/ninja_diff.md) explains
  key differences from Ninja.
- [Siso development](./docs/development.md) provides information for Siso developers.

## Status

Siso is the primary build system for Chromium and the projects that import //build from Chromium.

As of Aug 2025, Siso is moved to [go.chromium.org/build/siso](https://pkg.go.dev/go.chromium.org/build/siso).

As of June 2025, Siso is used in all the projects that import Chromium's //build, and is used by default on non-Google environments.

As of Apr 2025, Siso built-in remote exec client is used for Chromium and Chrome builders.

As of Nov 2024, Siso is used by default for Chromium build on gLinux machine.

As of July 2024, Siso is used in all Chromium and Chrome builders, including official
builds released to users.

As of end of 2024 Q1, Siso is used in all CQ builders in Chromium.

As of April 2023, we are dogfooding Siso with invited Chrome developers.
Please check [go/chrome-build-dogfood](http://go/chrome-build-dogfood) for more information.

## Contacts

- File a bug in [the public tracker](https://issues.chromium.org/issues/new?component=1724382&template=2146965) or in [the internal tracker](http://go/siso-bug).
- Ask a question in [#build](https://chromium.slack.com/archives/C08SJ9DH4BZ) Slack channel.
- Send an email to chrome-build-team@google.com.


## FAQ

Please check [go/siso-faq](http://go/siso-faq) (internal).

## References

* [Previous location of Siso's source](https://chromium.googlesource.com/infra/infra/+/9b440a2c2670568a7b6952d28ef5422e961be629/go/src/infra/build/siso) (infra)
