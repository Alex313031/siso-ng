module go.chromium.org/build/siso

// When updating the Go toolchain minor version, please check for OS compatibility
// with siso users.
//
// Go release history: https://go.dev/doc/devel/release
// Some siso user OS version is listed in http://shortn/_R9N9PLz4sg
go 1.24.10

require (
	cloud.google.com/go/compute/metadata v0.8.0
	cloud.google.com/go/logging v1.13.1
	cloud.google.com/go/longrunning v0.6.7
	cloud.google.com/go/profiler v0.4.3
	cloud.google.com/go/trace v1.11.6
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric v0.53.0
	github.com/Microsoft/go-winio v0.6.2
	github.com/bazelbuild/reclient/api v0.0.0-20240617160057-89d6134e48e5
	github.com/bazelbuild/remote-apis v0.0.0-20250915115802-824e1ba94b2d
	github.com/bazelbuild/remote-apis-sdks v0.0.0-20250818214745-5c719541ba4a
	github.com/biogo/hts v1.4.5
	github.com/golang/glog v1.2.5
	github.com/google/go-cmp v0.7.0
	github.com/google/subcommands v1.2.0
	github.com/google/uuid v1.6.0
	github.com/klauspost/compress v1.18.1
	github.com/klauspost/cpuid/v2 v2.3.0
	github.com/pkg/xattr v0.4.12
	go.chromium.org/build/kajiya v0.0.0-20251015062654-cd7695ac03de
	go.opentelemetry.io/otel v1.38.0
	go.opentelemetry.io/otel/metric v1.38.0
	go.opentelemetry.io/otel/sdk v1.38.0
	go.opentelemetry.io/otel/sdk/metric v1.38.0
	go.starlark.net v0.0.0-20250804182900-3c9dc17c5f2e
	golang.org/x/oauth2 v0.30.0
	golang.org/x/sync v0.17.0
	golang.org/x/sys v0.36.0
	golang.org/x/term v0.35.0
	google.golang.org/api v0.248.0
	google.golang.org/genproto v0.0.0-20250818200422-3122310a409c
	google.golang.org/genproto/googleapis/api v0.0.0-20250908214217-97024824d090
	google.golang.org/genproto/googleapis/bytestream v0.0.0-20250908214217-97024824d090
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250908214217-97024824d090
	google.golang.org/grpc v1.76.0
	google.golang.org/protobuf v1.36.10
)

require (
	cloud.google.com/go v0.121.6 // indirect
	cloud.google.com/go/auth v0.16.5 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.2.8 // indirect
	cloud.google.com/go/monitoring v1.24.2 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/internal/resourcemapping v0.53.0 // indirect
	github.com/GoogleCloudPlatform/protoc-gen-bq-schema v1.1.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/pprof v0.0.0-20250630185457-6e76a2b096b5 // indirect
	github.com/google/s2a-go v0.1.9 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.6 // indirect
	github.com/googleapis/gax-go/v2 v2.15.0 // indirect
	github.com/hashicorp/go-immutable-radix/v2 v2.1.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/contrib/detectors/gcp v1.38.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.62.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.62.0 // indirect
	go.opentelemetry.io/otel/trace v1.38.0 // indirect
	golang.org/x/crypto v0.42.0 // indirect
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	golang.org/x/time v0.12.0 // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.5.1 // indirect
)

tool (
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
)
