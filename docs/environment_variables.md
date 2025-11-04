# Siso environment variables.

Siso uses the following environment variables for configuration or flag
defaults. In addition to `SISO_*` variables, Siso partially supports
`RBE_*` variables for Reclient compatibility, and `NINJA_*` variables
for Ninja compatibility.

## Cloud control

### SISO_PROJECT

`SISO_PROJECT` sets the default value of `--project`, to specify Google
Cloud Project.

## Remote exec API control

### SISO_REAPI_INSTANCE

`SISO_REAPI_INSTANCE` sets the default value of `-reapi_instance`, to specify
RE API instance name.

### SISO_REAPI_ADDRESS

`SISO_REAPI_ADDRESS` sets the default value of `-reapi_address`, to specify
RE API service address.

### RBE_service_no_security

`RBE_service_no_security` sets the default value of `-reapi_insecure`,
when using RE API with no authentication.
(reclient compat)

### RBE_tls_client_auth_cert and RBE_tls_client_auth_key

`RBE_tls_clietn_auth_cert` and `RBE_tls_client_auth_key` sets
the default values of `-reapi_tls_client_auth_cert` and
`-reapi_tls_client_auth_key`, respectively, when to use mTLS authentication.
(reclient compat)

### RBE_tls_ca_cert

`RBE_tls_ca_cert` sets the default value of `-reapi_tls_ca_cert` to specify
Root CA Certificates file.
(reclient compat)

## Resource control

### SISO_LIMITS

`SISO_LIMITS` can control Siso's resource semaphore.
Comma separated key value pairs in the form of key=value
for experiments.

e.g. limit the local/remote concurrencies to 10/100 respectively.
`SISO_LIMITS=local=10,remote=100`

- `step`: maximum number of concurrent steps.
- `preproc`: maximum number of concurrent preprocesses.
- `scandeps`: maximum number of concurrent scandeps.
- `local`: maximum number of concurrent local executions.
      same with `--local_jobs`
- `fastlocal`: maximum number of local executions when local is idle
      even if step is remote executable.
- `startlocal`: maximum initial steps to run locally
      even if step is remote executable.
- `remote`: maximum number of concurrent remote executions.
      same with `--remote_jobs`
- `rewrap`: maximum number of concurrent `use_remote_exec_wrapper` steps.
- `cache`: maximum number of concurrent exec cache lookups.
- `thread`: maximum number of threads.

### NINJA_CORE_MULTIPLIER and NINJA_CORE_LIMIT

`NINJA_CORE_MULTIPLIER` and `NINJA_CORE_LIMIT` are used
to define default of `rewrap`'s limit for Reclient compatibility.

## Feature control

### SISO_CREDENTIAL_HELPER

`SISO_CREDENTIAL_HELPER` sets the default value of `-credential_helper`.
It specifies a path to a credential helper program.
See https://github.com/EngFlow/credential-helper-spec/blob/main/spec.md
See also [Siso authentication](./auth.md).

### RBE_remote_disabled

`RBE_remote_disabled` is used to decide offline mode or not.
(reclient compat)

### TERM

`TERM` is used to detect smart terminal.
`dump` or `dump-emacs-ansi` are not smart terminal.

### NO_COLOR

`NO_COLOR` is used to [disable colorted text output](https://no-color.org/).

### SISO_FSMONITOR

`SISO_FSMONITOR` is a path of file system monitoring tool.
Currently [`watchman`](https://facebook.github.io/watchman/) is supported.
(experimental).

### SISO_EXPERIMENTS

`SISO_EXPERIMENTS` is comma separated feature names.
See [build/experiments.go](../build/experiments.go) for available experiment
features.  The available experiments will change in future versions.

## Monitoring

### SISO_BUILD_ID

`SISO_BUILD_ID` sets the default value of `--build_id`, to identify
the build process.
Used for `tool_invocation_id` of remote apis, and `build_id` label of
Cloud logging resources.

### RBE_metrics_project
`RBE_metrics_project` sets the default value of `-metrics_project`,
to specify Google Cloud Project for cloud monitoring.
(reclient compat)

### RBE_metrics_labels
`RBE_metrics_labels` sets the default value of `-metrics_labels`,
which is comma separated arbitrary key value pairs in the form key=value
to be added for cloud monitoring metrics.
(reclient compat)

## Reclient support

The following environment variables are supported for reclient mode.
See https://github.com/bazelbuild/reclient/blob/main/docs/cmd-line-flags.md

### RBE_server_address

### RBE_exec_timeout

### RBE_reclient_timeout

### RBE_exec_strategy

### RBE_compare

### RBE_num_local_reruns

### RBE_num_remote_reruns

