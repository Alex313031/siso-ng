# Siso Development

This document is intended for Siso developers.

Also check [go/siso-development](http://go/siso-development) (internal).

## How to get the code

```
$ git clone https://chromium.googlesource.com/build
$ cd build/siso
```

## How to build the code

```
$ go install .
```

## How to test the Siso

```
$ go test ./...
```

To enable glog/clog output during tests, use the `-args` flag to pass glog flags:

```
$ go test -v ./... -args -logtostderr
```

Other glog flags can also be passed after `-args`. See
https://pkg.go.dev/github.com/golang/glog for more details.

To build chromium with your Siso,

```
# in chromium workspace
$ export SISO_PATH=$HOME/go/bin/siso
$ autoninja -C out/Default chrome
```

## How to update proto files.

When modified *.proto, or protobuf version.

```
$ go generate ./...
```

## How to update module dependencies.

e.g. to update `google.golang.org/grpc`

```
$ go get google.golang.org/grpc@v1.M.N
$ go mod tidy
```

To update `protoc-gen-go` or `protoc-gen-go-grpc`, use `-tool`.

```
$ go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@v1.M.N
$ go mod tidy
$ go generate ./...
```

## How to modify code with kajiya

When modifying code with kajiya, use
[go workspace](https://go.dev/doc/tutorial/workspaces).

At build repo checkout root,

```
$ go work init siso kajiya
```

Then, siso will use kajiya code in the checkout, not kajiya specified by go.mod.
So, modify code in kajiya and siso.

To land the change,

1.  land kajiya change.
2.  in siso, run `go get go.chromium.org/build/kajiya@latest` to update kajiya
    dependency for siso. check building siso by `GOWORK=off go install .` and
    `GOWORK=off go test ./...`
3.  `go mod tidy` to tidy up `go.mod` and `go.sum`.
4.  land siso change.

Better to sync dependencies by `go work sync` and `go mod tidy` in siso and
kajiya.

## How to use your favorite IDE

If you opened VS Code to the root of the build repo and you only work on Siso,
it's enough to create go workspace by:

```
$ go work init siso
```

This should be enough to make suggestions, find definition, etc work.

## Profiling

If you call Siso with the flag `-pprof_addr=localhost:6060`, it will start an
HTTP server on this socket that serves runtime profiling data. This is useful
for inspecting and debugging various aspects of Siso's performance. Please see
the documentation of [go tool pprof](https://pkg.go.dev/net/http/pprof) for
instructions and examples how to use it effectively.

Alternatively, you can use `-cpuprofile=cpu.prof` or `-memprofile=memory.prof`
to collect profiling data and save it to disk.

For using the commands mentioned above, you could add the flags into
[.sisorc](https://chromium.googlesource.com/chromium/src/+/HEAD/docs/siso_tips.md#preferred-command-line-flags)
so `autoninja` is able to pick it up when you build chromium.

> **Note**: If you get an error like `Error: could not find file
> go.chromium.org/build/siso/main.go ...` when using the `list` command in `go
> tool pprof`, it means `pprof` can't find the source files. This often happens
> if you run `go tool pprof` from a directory other than the `siso` source root.
> Try running `go tool pprof` from the `siso` directory.
>
> If that doesn't work, you can fix this by using `-source_path` to point to
> your source code and `-trim_path` to map from the package path.
>
> For example:
>
> ```sh
> go tool pprof -source_path=path/to/build -trim_path=go.chromium.org/build cpu.prof
> ```

## Tracing

Passing the flag `-trace=siso.trace` to Siso will cause it captures an execution
trace with a wide range of events that can be interpreted by the `go tool trace`
tool.
[The documentation of go tool trace](https://go.dev/blog/execution-traces-2024)
is a good place to check for ideas how to use this data.
