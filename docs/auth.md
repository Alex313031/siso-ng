# Siso authentication

Siso supports several authentication mechanisms, each with its own advantages
and use cases.

## For connection to RE API backend.

### Insecure mode
Set `--reapi_insecure` flag or `RBE_service_no_security=true` env var.

no TLS, no auth.  use it only in a closed network.

### mTLS (mutual TLS)

Set `--reapi_tls_client_auth_cert` and `--reapi_tls_client_auth_key` flags,
or `RBE_tls_client_auth_cert` and `RBE_tls_client_auth_key` env vars.

It will set `SISO_CREDENTIAL_HELPER=mTLS` to disable per RPC credentials.

### Non-standard TLS CA certs

Set `--reapi_tls_ca_cert` flag or `RBE_tls_ca_cert` env var.

## For per RPC credentials

### Bazel credential helper

Siso supports
[bazel credential helper](https://github.com/EngFlow/credential-helper-spec/blob/main/spec.md).

You can use your own credential helper by setting credential helper's path to
the `SISO_CREDENTIAL_HELPER` environment variable.

If your helper needs command line options, you may need a wrapper that supports
the bazel cred helper spec.

### Luci-auth

Siso supports the `luci-auth` command line tool by
`SISO_CREDENTIAL_HELPER=luci-auth`. Siso uses scopes
`cloud-platform` and `userinfo.email`, or same scopes for `luci-auth context`
(i.e. `--scopes-context`)

`luci-auth` is available in depot_tools

#### This app is blocked

Note: `luci-auth` might not be available for non Googlers. You'll get "This app
is blocked" error in auth flow. e.g. https://crbug.com/412384614 .

In this case, try `gcloud` instead.

### Gcloud

Siso supports the [gcloud](https://cloud.google.com/sdk/gcloud) command line
tool by `SISO_CREDENTIAL_HELPER=gcloud`.  need to [install gcloud
sdk](https://cloud.google.com/sdk/gcloud#download_and_install_the).

### Google Application Default Credentials

Siso supports [Google Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials)
by `SISO_CREDENTIAL_HELPER=google-application-default`.

## Note

Per RPC credentials are used not only for RE API backend access, but also for
Google cloud platform services, such as cloud logging, cloud monitoring, cloud
profiler, cloud tracing, resultstore etc. So,credential helper should return
valid Google OAuth2 access token for `{"uri":"https://*.googleapis.com/"}`, if
you use these cloud services.

`luci-auth`, `gcloud` and `google-application-default` are also for Google
platform, so it can be used for Google RBE and Google cloud platform.

If you're using a non-Google RE API backend, you'll need to use a credential
helper (or insecure, mTLS) for your RE API backend.

`siso login` only works for `luci-auth` or `gcloud`.  credential helper will
need its own login flow.

## Check auth status

You can check auth status by running `siso auth-check [-reapi_*]`

## Interoperability

reclient (>= 0.185.*) supports bazel credential helper
compatible format, and credshelper supports `-bazel_compat`.

Old reclient credshelper is slightly different from bazel credential
helper (timestamp format for expiry, etc).
To convert reclient credshelper's output to bazel credential helper,

```
import datetime
import json
import os
import subprocess

v = json.loads("<reclient credential helper's output>")
resp = {
    "headers":{"Authorization": [f"Bearer {v['token']}"]},
    "expires": datetime.datetime.strftime(datetime.datetime.strptime(v['expiry'], '%a %b %d %H:%M:%S UTC %Y'), '%Y-%m-%dT%H:%M:%SZ'),
}
print(json.dumps(resp))

```

`luci-auth` has `–json-format` option for `luci`, `reclient` or `bazel`,
but fixed version (v1.5.7) is not yet rolled out in depot_tools.
