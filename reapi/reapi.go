// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reapi provides remote execution API.
package reapi

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	otelmetric "go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	grpcexpotel "google.golang.org/grpc/experimental/opentelemetry"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	grpcotel "google.golang.org/grpc/stats/opentelemetry"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"
	semverpb "go.chromium.org/build/remote-apis/build/bazel/semver"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/o11y/monitoring"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/firstbyte"
	"go.chromium.org/build/siso/reapi/retry"
	"go.chromium.org/build/siso/sync/semaphore"
	"go.chromium.org/build/siso/version"

	_ "embed"
)

// Option contains options of remote exec API.
type Option struct {
	// Prefix is used to distinguish client.
	// If empty, "reapi" is used as prefix.
	// Usually it uses Address as endpoint,
	// and optionally CASAddress as endpoint for cas operation,
	// if it is explicitly specified by the flag.
	// If Prefix contains "cas", CASAddress will not be used.
	Prefix     string
	Address    string
	CASAddress string
	Instance   string

	// Insecure mode for RE API.
	Insecure bool

	// mTLS
	TLSClientAuthCert string
	TLSClientAuthKey  string

	TLSCACert string

	// ExecutionPriority sets the priority value to use when sending actions to the REAPI backend.
	//
	// This can be used, e.g., to prioritize interactive builds from developers over builds from CI.
	ExecutionPriority int

	// use compressed blobs if server supports compressed blobs and size is bigger than this.
	CompressedBlob int64
	// use compressed blobs in BatchUpdateBlobs if server supports it and size is bigger than this.
	// 0 means disabled.
	BatchCompressedBlob int64
	// compressor for ByteStream Read/Write APIs.
	compressor rpb.Compressor_Value
	// compressor for BatchUpdateBlobs API.
	compressorForBatchUpdateBlobs rpb.Compressor_Value

	// Threshold that decides whether to use ByteStream API (compression-aware)
	// instead of BatchReadBlobs if blob size is bigger than this.
	ByteStreamReadThreshold int64

	// Threshold that decides whether to use Chunked Blobs.
	ChunkedBlobsThreshold int64

	// threshold for batch for chunks.
	chunkBatchThreshold int64

	// Enables GRPC compression. If enabled, blob-level compression will be
	// forcibly disabled.
	EnableGRPCCompression bool

	// Keep Execute stream open as lone as possible.
	// If false, siso closes Execute stream every 1 minute and retries
	// with WaitExecution to mitigate grpc/network issue.
	KeepExecStream bool

	// Use GetTree for WalkDir.
	// If false, use BatchReadBlobs instead.
	WalkDirStream bool

	ConnPool        int
	DataConnPool    int
	KeepAliveParams keepalive.ClientParameters

	// StatsHandler, if non-nil, is wired into every gRPC ClientConn via
	// grpc.WithStatsHandler. Used by the resource package to sample TTFB
	// for the adaptive flush gate.
	StatsHandler stats.Handler

	// RE API version to use by siso, in format of v<major>.<minor>
	// e.g. "v2.0".
	// default to use the highest version that both siso and the server
	// support.
	REAPIVersion string

	// DigestFunction selects the content digest function by name
	// (e.g. "sha256", "sha1", "blake3"). Empty means "sha256".
	DigestFunction string

	// UploadConcurrency caps in-flight upload RPCs per upload request.
	// Zero (default) means serial; set to max(32, GOMAXPROCS*4) or similar
	// for callers that benefit from parallel upload (e.g. `siso isolate`).
	UploadConcurrency int

	// UploadLimit caps the number of UploadAll requests in flight at
	// once (the "reapi-upload" semaphore). Zero means DefaultUploadLimit.
	UploadLimit int

	// MaxRetries is the maximum number of retries for retriable errors.
	MaxRetries int

	// TracerProvider, when non-nil, enables grpc-native OpenTelemetry tracing.
	// Each RPC propagates its context via grpc-trace-bin for server-side
	// recording (e.g. Dapper). Setting it replaces gtransport's default
	// telemetry handler.
	TracerProvider oteltrace.TracerProvider

	// MeterProvider, when non-nil, enables grpc-native OpenTelemetry client
	// metrics through this provider. Setting it replaces gtransport's default
	// telemetry handler.
	MeterProvider otelmetric.MeterProvider

	// TraceCookie, when non-empty, is attached as the `cookie` gRPC metadata
	// header on every RPC to this backend (to force server-side trace
	// sampling, e.g. Dapper). Scoped to this connection so it never reaches a
	// non-RBE gRPC service.
	TraceCookie string

	// LocalCache is local cache for reapi.
	LocalCache *LocalCache

	// Disable Two Phase Caching method (for test).
	// TODO: implement Two Phase Caching method in kajiya
	DisableTwoPhaseCachingMethods bool
}

// Envs returns environment flags for reapi.
func Envs(t string) map[string]string {
	envs := map[string]string{}
	if v, ok := os.LookupEnv(fmt.Sprintf("SISO_%s_INSTANCE", t)); ok {
		envs["SISO_REAPI_INSTANCE"] = v
	}
	if v, ok := os.LookupEnv(fmt.Sprintf("SISO_%s_ADDRESS", t)); ok {
		envs["SISO_REAPI_ADDRESS"] = v
	}
	if v, ok := os.LookupEnv(fmt.Sprintf("SISO_%s_CAS_ADDRESS", t)); ok {
		envs["SISO_REAPI_CAS_ADDRESS"] = v
	}
	return envs
}

// RegisterFlags registers flags on the option.
// Note: if Prefix contains "cas", it would only be used for cas,
// so not register additional cas address flags.
func (o *Option) RegisterFlags(fs *flag.FlagSet, envs map[string]string) {
	var purpose string
	if o.Prefix == "" {
		o.Prefix = "reapi"
	} else {
		purpose = fmt.Sprintf(" (for %s)", o.Prefix)
	}
	addr := envs["SISO_REAPI_ADDRESS"]
	if addr == "" {
		addr = "remotebuildexecution.googleapis.com:443"
	}
	fs.StringVar(&o.Address, o.Prefix+"_address", addr, "reapi address"+purpose)
	if !strings.Contains(o.Prefix, "cas") {
		casAddr := envs["SISO_REAPI_CAS_ADDRESS"]
		fs.StringVar(&o.CASAddress, o.Prefix+"_cas_address", casAddr, "reapi cas address"+purpose+" (if empty, share conn with "+o.Prefix+"_address)")
	}
	instance, ok := envs["SISO_REAPI_INSTANCE"]
	if !ok {
		instance = "default_instance"
	}
	usage := "reapi instance name" + purpose
	if o.Prefix == "reapi" {
		usage += ". (for Google RBE: if instance is fully qualified (starts with projects/), project ID is inferred from it. Otherwise, project ID from -project (or $SISO_PROJECT) is used to construct the instance name. If both are provided, -project is used for other cloud services)"
	}
	fs.StringVar(&o.Instance, o.Prefix+"_instance", instance, usage)

	fs.BoolVar(&o.Insecure, o.Prefix+"_insecure", os.Getenv("RBE_service_no_security") == "true", "reapi insecure mode. default can be set by $RBE_service_no_security")

	fs.StringVar(&o.TLSClientAuthCert, o.Prefix+"_tls_client_auth_cert", os.Getenv("RBE_tls_client_auth_cert"), "Certificate to use when using mTLS to connect to the RE api service. default can be set by $RBE_tls_client_auth_cert")
	fs.StringVar(&o.TLSClientAuthKey, o.Prefix+"_tls_client_auth_key", os.Getenv("RBE_tls_client_auth_key"), "Key to use when using mTLS to connect to the RE api service. default can be set by $RBE_tls_client_auth_key")

	fs.StringVar(&o.TLSCACert, o.Prefix+"_tls_ca_cert", os.Getenv("RBE_tls_ca_cert"), "Load TLS CA certificates from this file to connect to the RE api service. default can be set by $RBE_tls_ca_cert")

	fs.Int64Var(&o.CompressedBlob, o.Prefix+"_compress_blob", 1024, "use compressed blobs if server supports compressed blobs and size is bigger than this. specify 0 to disable blob-level compression."+purpose)

	fs.Int64Var(&o.BatchCompressedBlob, o.Prefix+"_batch_compress_blob", 0, "use compressed blobs in BatchUpdateBlobs if server supports it and size is bigger than this. specify 0 to disable."+purpose)

	fs.Int64Var(&o.ByteStreamReadThreshold, o.Prefix+"_byte_stream_read_threshold", 1024*1024, "if blob size >= threshold, use ByteStream API (compression-aware)"+purpose)

	fs.Int64Var(&o.ChunkedBlobsThreshold, o.Prefix+"_chunked_blobs_threshold", 0, "If blob size >= threshold, try to use chunked blob)"+purpose)

	fs.BoolVar(&o.EnableGRPCCompression, o.Prefix+"_enable_grpc_compression", false, "enable grpc compression.  if enabled, blob-level compression will be forcibly disabled."+purpose)

	fs.BoolVar(&o.KeepExecStream, o.Prefix+"_keep_exec_stream", false, "keep Execute stream open as long as possible")

	fs.BoolVar(&o.WalkDirStream, o.Prefix+"_walkdir_stream", false, "use GetTree for WalkDir")

	fs.IntVar(&o.ConnPool, o.Prefix+"_grpc_conn_pool", 25, "grpc connection pool")
	fs.IntVar(&o.DataConnPool, o.Prefix+"_grpc_data_conn_pool", 25, "grpc connection pool for data (bytestream Read, Write)")

	// https://grpc.io/docs/guides/keepalive/#keepalive-configuration-specification
	// b/286237547 - RBE suggests 30s
	fs.DurationVar(&o.KeepAliveParams.Time, o.Prefix+"_grpc_keepalive_time", 30*time.Second, "grpc keepalive time"+purpose)
	fs.DurationVar(&o.KeepAliveParams.Timeout, o.Prefix+"_grpc_keepalive_timeout", 20*time.Second, "grpc keepalive timeout"+purpose)
	fs.BoolVar(&o.KeepAliveParams.PermitWithoutStream, o.Prefix+"_grpc_keepalive_permit_without_stream", false, "grpc keepalive permit without stream"+purpose)

	fs.StringVar(&o.REAPIVersion, o.Prefix+"_version_to_use", "", "specify re api version to use, in format of v<major>.<minor>. e.g. v2.0. default: the highest version both siso and the server support")

	fs.StringVar(&o.DigestFunction, o.Prefix+"_digest_function", "sha256", "content digest function: sha256, sha1, gitsha1, blake3, md5, sha384, sha512, murmur3, vso, or sha256tree"+purpose)

	fs.StringVar(&o.TraceCookie, o.Prefix+"_trace_cookie", "", "if set, sent as the `cookie` gRPC metadata header on every RPC to this backend, to force server-side trace sampling (e.g. Dapper)"+purpose)

	// Flags only supported for "execution".
	if o.Prefix == "reapi" {
		fs.IntVar(&o.ExecutionPriority, o.Prefix+"_priority", 0, "reapi priority for action executions"+purpose+". The semantics and supported values depend on the backend")
	}
	fs.IntVar(&o.MaxRetries, o.Prefix+"_max_retries", 10, "max retries for remote execution")
}

func isGoogleRBE(address string) bool {
	return strings.HasSuffix(address, "remotebuildexecution.googleapis.com:443") || strings.HasSuffix(address, "remotebuildexecution.sandbox.googleapis.com:443")
}

func (o *Option) String() string {
	if o == nil || o.Address == "" {
		return "no reapi backend"
	}
	addr := fmt.Sprintf("reapi %q", o.Address)
	if isGoogleRBE(o.Address) {
		addr = "RBE"
		switch {
		case strings.HasSuffix(o.Address, "-remotebuildexecution.googleapis.com:443"):
			addr = fmt.Sprintf("RBE(%s)", strings.TrimSuffix(o.Address, "-remotebuildexecution.googleapis.com:443"))
		case strings.HasSuffix(o.Address, "-remotebuildexecution.sandbox.googleapis.com:443"):
			addr = fmt.Sprintf("RBE(%s sandbox)", strings.TrimSuffix(o.Address, "-remotebuildexecution.sandbox.googleapis.com:443"))
		}
	}
	return fmt.Sprintf("%s instance %q", addr, o.Instance)
}

// UpdateProjectID updates the Option for projID and returns cloud project ID to use.
// Just returns original projID if backend is not RBE.
func (o *Option) UpdateProjectID(projID string) string {
	if !isGoogleRBE(o.Address) {
		return projID
	}
	if projID != "" && !strings.HasPrefix(o.Instance, "projects/") {
		o.Instance = path.Join("projects", projID, "instances", o.Instance)
	}
	if projID == "" && strings.HasPrefix(o.Instance, "projects/") {
		projID = strings.Split(o.Instance, "/")[1]
	}
	if projID == "" && !strings.HasPrefix(o.Instance, "projects/") {
		// make Option invalid.
		o.Instance = ""
	}
	return projID
}

// CheckValid checks whether option is valid or not.
func (o Option) CheckValid() error {
	if o.Address == "" {
		return errors.New("no reapi address")
	}
	if isGoogleRBE(o.Address) && o.Instance == "" {
		return errors.New("no reapi instance for Google RBE")
	}
	return nil
}

// NeedCred returns whether credential is needed or not.
func (o Option) NeedCred() bool {
	if o.Address == "" {
		return false
	}
	if o.CheckValid() != nil {
		return false
	}
	if o.Insecure {
		return false
	}
	if o.TLSClientAuthCert != "" || o.TLSClientAuthKey != "" {
		return false
	}
	return true
}

// ServiceURI returns service uri (capabilities) for PerRPCCredentials
// to check auth in cred.New
func (o Option) ServiceURI() string {
	uri := o.Address
	if uri == "" {
		return ""
	}
	if !strings.HasPrefix(uri, "http") {
		method := "http"
		if strings.HasSuffix(uri, ":443") {
			method = "https"
			uri = strings.TrimSuffix(uri, ":443")
		}
		uri = fmt.Sprintf("%s://%s", method, uri)
	}
	uri += rpb.Capabilities_GetCapabilities_FullMethodName
	return uri
}

type grpcClientConn interface {
	grpc.ClientConnInterface
	io.Closer
}

// Client is a remote exec API client.
type Client struct {
	opt         Option
	digestFn    digest.Function
	cred        cred.Cred
	conn        grpcClientConn
	casConn     grpcClientConn
	casDataConn grpcClientConn

	mu           sync.Mutex
	capabilities *rpb.ServerCapabilities
	apiVersion   Version

	knownDigests sync.Map // key:digest.Digest, value: *uploadOp or true

	// uploader runs the CAS upload requests made by UploadAll, owned by
	// the Client (lifetime derived from the context given to
	// New/NewFromConn, closed by Close), so in-flight uploads are
	// canceled when the build stops, not when an individual requester
	// gives up.
	uploader *uploader

	zstdDecoderPool *sync.Pool

	m *iometrics.IOMetrics

	walkdirCache sync.Map // key:digest.Digest, value:*rpb.Directory, shared; must not be modified
}

// serviceConfig is gRPC service config for RE API.
// https://github.com/bazelbuild/bazel/blob/7.1.1/src/main/java/com/google/devtools/build/lib/remote/RemoteRetrier.java#L47
//
//go:embed service_config.json
var serviceConfig string

// keepaliveDialOptions returns the dial options that enable client keepalive
// with params, or none if every parameter is zero, which disables keepalive.
// grpc.WithKeepaliveParams can't express that: grpc-go raises a zero interval
// to its 10 s minimum.
func keepaliveDialOptions(params keepalive.ClientParameters) []grpc.DialOption {
	if params.Time == 0 && params.Timeout == 0 && !params.PermitWithoutStream {
		return nil
	}
	return []grpc.DialOption{grpc.WithKeepaliveParams(params)}
}

func DialOptions(keepAliveParams keepalive.ClientParameters) []grpc.DialOption {
	// TODO(b/273639326): handle auth failures gracefully.

	// https://github.com/grpc/grpc/blob/c16338581dba2b054bf52484266b79e6934bbc1c/doc/service_config.md
	// https://github.com/grpc/proposal/blob/9f993b522267ed297fe54c9ee32cfc13699166c7/A6-client-retries.md
	// timeout=300s may cause deadline exceeded to fetch large *.so file?
	var dopts []grpc.DialOption
	dopts = append(dopts, keepaliveDialOptions(keepAliveParams)...)
	dopts = append(dopts,
		grpc.WithDisableServiceConfig(),
		grpc.WithDefaultServiceConfig(serviceConfig),
		// active streams and per-RPC stages into bytestream.* histograms
		grpc.WithStatsHandler(monitoring.BytestreamStatsHandler()),
		// closes firstbyte.Signal on first InPayload (used by hashfs's
		// pre-first-byte watchdog)
		grpc.WithStatsHandler(firstbyte.Handler),
	)
	return dopts
}

// otelDialOption installs the requested grpc-native OpenTelemetry tracing and
// metrics instrumentation.
func otelDialOption(tp oteltrace.TracerProvider, mp otelmetric.MeterProvider) grpc.DialOption {
	var opt grpcotel.Options
	if tp != nil {
		opt.TraceOptions = grpcexpotel.TraceOptions{
			TracerProvider:    tp,
			TextMapPropagator: grpcotel.GRPCTraceBinPropagator{},
		}
	}
	if mp != nil {
		opt.MetricsOptions = grpcotel.MetricsOptions{
			MeterProvider: mp,
			// Keep the fleet-visible metric set stable across grpc-go upgrades.
			Metrics: stats.NewMetricSet(
				grpcotel.ClientAttemptStartedMetricName,
				grpcotel.ClientAttemptDurationMetricName,
				grpcotel.ClientAttemptSentCompressedTotalMessageSizeMetricName,
				grpcotel.ClientAttemptRcvdCompressedTotalMessageSizeMetricName,
				grpcotel.ClientCallDurationMetricName,
			),
		}
	}
	return grpcotel.DialOption(opt)
}

// cookieInterceptors return unary and stream client interceptors that attach
// cookie as the `cookie` gRPC metadata header to every RPC on the connection.
// Scoping it here keeps the (debug) cookie off any non-RBE gRPC service.
func cookieInterceptors(cookie string) (grpc.UnaryClientInterceptor, grpc.StreamClientInterceptor) {
	unary := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.AppendToOutgoingContext(ctx, "cookie", cookie), method, req, reply, cc, opts...)
	}
	stream := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(metadata.AppendToOutgoingContext(ctx, "cookie", cookie), desc, cc, method, opts...)
	}
	return unary, stream
}

// DialError is reapi dial error.
type DialError struct {
	Err error
}

func (e DialError) Error() string {
	return fmt.Sprintf("%v", e.Err)
}

func (e DialError) Unwrap() error {
	return e.Err
}

// New creates new remote exec API client.
func New(ctx context.Context, cred cred.Cred, opt Option) (*Client, error) {
	defer trace.Begin(ctx, "reapi.New").End()
	if opt.Address == "" {
		return nil, DialError{Err: errors.New("no reapi address")}
	}
	if isGoogleRBE(opt.Address) && opt.Instance == "" {
		return nil, DialError{Err: errors.New("no reapi instance")}
	}
	if opt.EnableGRPCCompression && (opt.CompressedBlob != 0 || opt.BatchCompressedBlob != 0) {
		opt.CompressedBlob = 0
		opt.BatchCompressedBlob = 0
		clog.Warningf(ctx, "disabling blob compression because grpc compression is enabled")
	}
	address := opt.Address
	clog.Infof(ctx, "address: %q (%d) instance: %q", address, opt.ConnPool, opt.Instance)
	conn, err := newConn(ctx, address, cred, opt.ConnPool, opt)
	if err != nil {
		return nil, DialError{Err: err}
	}
	casConn := conn
	if opt.CASAddress != "" {
		address = opt.CASAddress
		clog.Infof(ctx, "cas address: %q (%d)", address, opt.ConnPool)
		casConn, err = newConn(ctx, address, cred, opt.ConnPool, opt)
		if err != nil {
			conn.Close()
			return nil, DialError{Err: err}
		}
	}
	casDataConn := casConn
	if opt.DataConnPool > 0 {
		clog.Infof(ctx, "data address: %q (%d)", address, opt.DataConnPool)
		casDataConn, err = newConn(ctx, address, cred, opt.DataConnPool, opt)
		if err != nil {
			conn.Close()
			if conn != casConn {
				casConn.Close()
			}
			return nil, DialError{Err: err}
		}
	}
	return NewFromConn(ctx, opt, cred, conn, casConn, casDataConn)
}

func newConn(ctx context.Context, addr string, cred cred.Cred, pool int, opt Option) (grpcClientConn, error) {
	// Force the gRPC DNS resolver by prefixing "dns:///" when the caller
	// did not supply a scheme. gtransport.DialPool rides the deprecated
	// grpc.DialContext path whose default resolver is "passthrough",
	// which treats the target as a single opaque address. Under
	// passthrough, the round_robin LB policy in serviceConfig only ever
	// gets one subchannel per ClientConn, even though DNS returns many
	// GFE VIPs. "dns:///" forces the DNS resolver regardless of which
	// dial API sits underneath, so round_robin can fan out one
	// subchannel per resolved address.
	endpoint := addr
	if !strings.Contains(addr, "://") {
		endpoint = "dns:///" + addr
	}
	copts := []option.ClientOption{
		option.WithEndpoint(endpoint),
		option.WithGRPCConnectionPool(pool),
	}
	if !isGoogleRBE(addr) {
		// disable Google Application Default for non RBE backend.
		// user should specify credential helper for the backend.
		copts = append(copts, option.WithoutAuthentication())
	}
	keepAliveParams := opt.KeepAliveParams
	if strings.HasPrefix(addr, "unix://") {
		// Unix Domain Sockets operate in local kernel memory on the same host.
		// Disconnects are signaled immediately by the OS via EOF/POLLHUP, and there
		// are no NAT middleboxes or firewalls. Disabling gRPC keepalive on UDS
		// prevents false-positive PING ACK timeouts under high local build load.
		clog.Infof(ctx, "disabling keepalive on unix domain socket: %q", addr)
		keepAliveParams = keepalive.ClientParameters{}
	}
	dopts := DialOptions(keepAliveParams)
	if opt.TracerProvider != nil || opt.MeterProvider != nil {
		// gtransport combines tracing and metrics in one handler. Disable it
		// whenever either is replaced to avoid duplicate telemetry.
		copts = append(copts, option.WithTelemetryDisabled())
		dopts = append(dopts, otelDialOption(opt.TracerProvider, opt.MeterProvider))
	}
	if opt.TraceCookie != "" {
		unary, stream := cookieInterceptors(opt.TraceCookie)
		dopts = append(dopts, grpc.WithChainUnaryInterceptor(unary), grpc.WithChainStreamInterceptor(stream))
	}
	if opt.StatsHandler != nil {
		dopts = append(dopts, grpc.WithStatsHandler(opt.StatsHandler))
	}
	if opt.EnableGRPCCompression {
		dopts = append(dopts, grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)))
	}
	var conn grpcClientConn
	var err error
	var tlsConfig *tls.Config
	if opt.Insecure {
		// Insecure mode for non-RBE remote execution API.
		if strings.HasSuffix(addr, ".googleapis.com:443") {
			return nil, errors.New("insecure mode is not supported for RBE")
		}
		clog.Warningf(ctx, "insecure mode")
		copts = append(copts, option.WithoutAuthentication())
		dopts = append(dopts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		for _, dopt := range dopts {
			copts = append(copts, option.WithGRPCDialOption(dopt))
		}
		conn, err = gtransport.DialInsecure(ctx, copts...)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %s: %w", addr, err)
		}
		return conn, nil
	}

	copts = append(copts, cred.ClientOptions()...)

	if opt.TLSCACert != "" {
		clog.Infof(ctx, "using TLS CA certificates=%q", opt.TLSCACert)
		certPool := x509.NewCertPool()
		ca, err := os.ReadFile(opt.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("failed to read TLS CA certificates %q: %w", opt.TLSCACert, err)
		}
		if ok := certPool.AppendCertsFromPEM(ca); !ok {
			return nil, fmt.Errorf("failed to load TLS CA certificates from %s", opt.TLSCACert)
		}
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}
		tlsConfig.RootCAs = certPool
	}

	if opt.TLSClientAuthCert != "" && opt.TLSClientAuthKey != "" {
		// use mTLS certificates for authentication.
		clog.Infof(ctx, "using mTLS: cert=%q key=%q", opt.TLSClientAuthCert, opt.TLSClientAuthKey)
		cert, err := tls.LoadX509KeyPair(opt.TLSClientAuthCert, opt.TLSClientAuthKey)
		if err != nil {
			return nil, fmt.Errorf("failed to read mTLS cert pair (%q, %q): %w", opt.TLSClientAuthCert, opt.TLSClientAuthKey, err)
		}
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	} else if opt.TLSClientAuthCert != "" {
		return nil, errors.New("tls_client_auth_cert is set, but tls_client_auth_key is not set")
	} else if opt.TLSClientAuthKey != "" {
		return nil, errors.New("tls_client_auth_key is set, but tls_client_auth_cert is not set")
	}
	if tlsConfig != nil {
		copts = append(copts, option.WithGRPCDialOption(grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))))
	}
	for _, dopt := range dopts {
		copts = append(copts, option.WithGRPCDialOption(dopt))
	}
	conn, err = gtransport.DialPool(ctx, copts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %w", addr, err)
	}
	return conn, nil
}

// NewFromConn creates new remote exec API client from conn, casConn, and casDataConn.
func NewFromConn(ctx context.Context, opt Option, cred cred.Cred, conn, casConn, casDataConn grpcClientConn) (*Client, error) {
	zstdDecoderPool := &sync.Pool{}
	zstdDecoderPool.New = func() any {
		d, err := zstd.NewReader(nil, zstdDecoderOpts...)
		if err != nil {
			clog.Fatalf(ctx, "failed to create zstd.Decoder: %v", err)
		}
		pd := &pooledDecoder{
			Decoder: d,
			pool:    zstdDecoderPool,
		}
		return pd
	}
	fn, err := ParseDigestFunction(opt.DigestFunction)
	if err != nil {
		conn.Close()
		if casConn != nil && casConn != conn {
			casConn.Close()
		}
		if casDataConn != nil && casDataConn != casConn {
			casDataConn.Close()
		}
		return nil, err
	}
	c := &Client{
		opt:             opt,
		digestFn:        fn,
		cred:            cred,
		conn:            conn,
		casConn:         casConn,
		casDataConn:     casDataConn,
		zstdDecoderPool: zstdDecoderPool,
		m:               iometrics.New("reapi"),
	}
	c.knownDigests.Store(c.digestFn.Empty(), true)
	c.uploader = newUploader(ctx, c, opt.UploadLimit)
	return c, nil
}

// ParseDigestFunction resolves the content digest function from the
// -..._digest_function flag value. An empty name means sha256.
func ParseDigestFunction(name string) (digest.Function, error) {
	if name == "" {
		name = "sha256"
	}
	return digest.ParseFunction(name)
}

// DigestFunction returns the content digest function used with this backend.
func (c *Client) DigestFunction() digest.Function {
	return c.digestFn
}

// Init initializes the client by fetching capabilities and negotiating compression.
// This requires an active connection to the remote execution backend.
func (c *Client) Init(ctx context.Context) error {
	defer trace.Begin(ctx, "reapi.Init").End()
	err := func() error {
		defer trace.Begin(ctx, "reapi cred.Wait").End()
		return c.cred.Wait()
	}()
	if err != nil {
		return fmt.Errorf("failed to initialize credentials: %w", err)
	}

	cc := rpb.NewCapabilitiesClient(c.conn)
	var capa *rpb.ServerCapabilities
	// TODO(b/328332495): grpc should retry by service config?
	err = retry.Do(ctx, func() error {
		defer trace.Begin(ctx, "reapi GetCapabilities").End()
		var err error
		capa, err = cc.GetCapabilities(ctx, &rpb.GetCapabilitiesRequest{
			InstanceName: c.opt.Instance,
		})
		return err
	})
	if err != nil {
		c.Close()
		return fmt.Errorf("failed to get capabilities: %w", err)
	}
	clog.Infof(ctx, "capabilities of %s: %s", c.opt.Instance, capa)
	if err := validateDigestFunction(c.digestFn, capa); err != nil {
		c.Close()
		return err
	}
	compressedBlob := c.opt.CompressedBlob
	var compressor rpb.Compressor_Value
	if c.opt.CompressedBlob > 0 {
		compressor = selectCompressor(capa.GetCacheCapabilities().GetSupportedCompressors())
		if compressor != rpb.Compressor_IDENTITY {
			clog.Infof(ctx, "compressed-blobs/%s for > %d", strings.ToLower(compressor.String()), c.opt.CompressedBlob)
		} else {
			clog.Infof(ctx, "compressed-blobs is not supported")
			compressedBlob = 0
		}
	}
	batchCompressedBlob := c.opt.BatchCompressedBlob
	var compressorForBatchUpdateBlobs rpb.Compressor_Value
	if c.opt.BatchCompressedBlob > 0 {
		compressorForBatchUpdateBlobs = selectCompressor(capa.GetCacheCapabilities().GetSupportedBatchUpdateCompressors())
		if compressorForBatchUpdateBlobs != rpb.Compressor_IDENTITY {
			clog.Infof(ctx, "batch-update-blobs/%s for > %d", strings.ToLower(compressorForBatchUpdateBlobs.String()), c.opt.BatchCompressedBlob)
		} else {
			clog.Infof(ctx, "batch-update-blobs compression is not supported")
			batchCompressedBlob = 0
		}
	}
	clog.Infof(ctx, "byte stream read threshold: %d", c.opt.ByteStreamReadThreshold)
	apiVersion, err := negotiateAPIVersion(capa, c.opt.REAPIVersion)
	if err != nil {
		c.Close()
		return err
	}
	// prefer compressed bytestream.
	chunkBatchThreshold := c.opt.CompressedBlob
	if c.opt.BatchCompressedBlob > 0 {
		// if batch compressed is enabled, use max_batch_total_size_bytes for batching.
		chunkBatchThreshold = defaultBatchReadByteLimit
		if max := c.capabilities.GetCacheCapabilities().GetMaxBatchTotalSizeBytes(); max > 0 {
			chunkBatchThreshold = max
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capabilities = capa
	c.apiVersion = apiVersion
	c.opt.CompressedBlob = compressedBlob
	c.opt.compressor = compressor
	c.opt.BatchCompressedBlob = batchCompressedBlob
	c.opt.compressorForBatchUpdateBlobs = compressorForBatchUpdateBlobs
	c.opt.chunkBatchThreshold = chunkBatchThreshold
	return nil
}

// validateDigestFunction checks that the configured digest function is
// advertised by the server's cache capabilities and, when the server offers
// remote execution, by its execution capabilities too (otherwise CAS uploads
// would succeed but Execute would fail later). An empty advertised list means
// an old server that only speaks sha256: clients infer sha256 in that case,
// but any other function is rejected.
func validateDigestFunction(fn digest.Function, capa *rpb.ServerCapabilities) error {
	if err := checkAdvertisedDigestFunctions(fn, capa.GetCacheCapabilities().GetDigestFunctions(), "cache"); err != nil {
		return err
	}
	ec := capa.GetExecutionCapabilities()
	if !ec.GetExecEnabled() {
		return nil
	}
	advertised := ec.GetDigestFunctions()
	if len(advertised) == 0 && ec.GetDigestFunction() != rpb.DigestFunction_UNKNOWN {
		// Per the spec, the repeated digest_functions field takes precedence,
		// falling back to the legacy singular field if it is unset.
		advertised = []rpb.DigestFunction_Value{ec.GetDigestFunction()}
	}
	return checkAdvertisedDigestFunctions(fn, advertised, "execution")
}

// checkAdvertisedDigestFunctions checks fn against one capability's advertised
// digest functions.
func checkAdvertisedDigestFunctions(fn digest.Function, advertised []rpb.DigestFunction_Value, capability string) error {
	if len(advertised) == 0 {
		if fn == digest.SHA256 {
			return nil
		}
		return fmt.Errorf("digest function %s not supported; server advertised no %s digest functions", fn, capability)
	}
	if slices.Contains(advertised, fn.Value()) {
		return nil
	}
	return fmt.Errorf("digest function %s is not supported by the server's %s capabilities; advertised: %v", fn, capability, advertised)
}

// Close cancels in-flight upload requests, stops the upload workers,
// and closes the client's connections, including the separate CAS
// connection when one was dialed.
func (c *Client) Close() error {
	if c.uploader != nil {
		c.uploader.close(ErrClientClosed)
	}
	err := c.conn.Close()
	if c.casConn != nil && c.casConn != c.conn {
		cerr := c.casConn.Close()
		if err == nil {
			err = cerr
		}
	}
	if c.casDataConn != nil && c.casDataConn != c.casConn {
		cerr := c.casDataConn.Close()
		if err == nil {
			err = cerr
		}
	}
	return err
}

// UploadSemaphore returns the semaphore that bounds in-flight UploadAll
// requests, for stats. It returns nil for a nil client.
func (c *Client) UploadSemaphore() *semaphore.Semaphore {
	if c == nil || c.uploader == nil {
		return nil
	}
	return c.uploader.sema
}

// IOMetrics returns an IOMetrics of the client.
func (c *Client) IOMetrics() *iometrics.IOMetrics {
	if c == nil {
		return nil
	}
	return c.m
}

// Proto fetches contents of digest into proto message.
func (c *Client) Proto(ctx context.Context, d digest.Digest, p proto.Message) error {
	if c != nil && c.opt.LocalCache != nil {
		err := c.opt.LocalCache.Proto(ctx, d, p)
		if err == nil {
			return nil
		}
	}
	b, err := c.Get(ctx, d, fmt.Sprintf("%s -> %T", d, p))
	if err != nil {
		return err
	}
	err = proto.Unmarshal(b, p)
	if err != nil {
		return err
	}
	if c != nil && c.opt.LocalCache != nil {
		err = c.opt.LocalCache.SetProto(ctx, d, p)
		if err != nil {
			clog.Warningf(ctx, "store proto %s: %v", d, err)
		}
	}
	return nil
}

// GetActionResultTimeout caps a single GetActionResult attempt.
// 1.2s from chrome's p95 seems to be too short. b/540566219
// Cancel-and-retry on a fresh stream beats waiting toward the 10s
// service-config deadline.
// Applied to attempt 0 only; retries run to the natural 10s deadline.
// Set to 0 to disable.
var GetActionResultTimeout = 2 * time.Second

// keepFirstAttempt reports whether to return the first GetActionResult
// attempt or fall back to a retry. The deadline can fire in the gap
// between the call returning and this check, so a completed attempt
// (success or final error) is kept; we fall back only when our own
// timeout actually cut the call off with DeadlineExceeded.
func keepFirstAttempt(callCtx context.Context, timeoutCause, callErr error) bool {
	cutByOurTimeout := errors.Is(context.Cause(callCtx), timeoutCause) &&
		status.Code(callErr) == codes.DeadlineExceeded
	return !cutByOurTimeout
}

// ValidateActionResult checks whether the action result is valid.
func ValidateActionResult(result *rpb.ActionResult) bool {
	if result == nil {
		return false
	}
	if result.ExitCode == 0 && len(result.GetOutputFiles()) == 0 && len(result.GetOutputDirectories()) == 0 && len(result.GetOutputSymlinks()) == 0 &&
		len(result.GetOutputFileSymlinks()) == 0 && len(result.GetOutputDirectorySymlinks()) == 0 { //nolint:staticcheck // existing deprecation
		// succeeded result should have at least one output. b/350360391
		// A dir-only output has no OutputFiles but does have an
		// OutputDirectory, so accept that too (else it re-executes every build).
		return false
	}
	return true
}

// GetActionResult gets the action result by the digest.
func (c *Client) GetActionResult(ctx context.Context, d digest.Digest) (*rpb.ActionResult, error) {
	if c != nil && c.opt.LocalCache != nil {
		ar, err := c.opt.LocalCache.GetActionResult(ctx, d)
		if err == nil {
			if ar.ExitCode != 0 || ValidateActionResult(ar) {
				return ar, nil
			}
		}
	}
	client := rpb.NewActionCacheClient(c.casConn)
	req := &rpb.GetActionResultRequest{
		InstanceName:   c.opt.Instance,
		ActionDigest:   d.Proto(),
		DigestFunction: c.digestFn.Value(),
	}
	// Attempt 0 runs under a short deadline so a stalled lookup
	// doesn't sit the full 10s service-config deadline. Only our
	// synthetic Aborted cause triggers the fallback attempt; any
	// other error is returned as-is, since gRPC method-config retry
	// (Aborted/Internal/ResourceExhausted/Unavailable/Unknown, up to
	// 5 attempts) already covered the transport-level retries.
	retried := false
	if GetActionResultTimeout > 0 {
		cause := status.Error(codes.Aborted, "GetActionResult first-byte timeout")
		callCtx, cancel := context.WithTimeoutCause(ctx, GetActionResultTimeout, cause)
		result, err := client.GetActionResult(callCtx, req)
		// Decide before cancel() overwrites the deadline cause.
		keep := keepFirstAttempt(callCtx, cause, err)
		cancel()
		clog.Infof(ctx, "GetActionResult keep=%t err=%v", keep, err)
		if keep {
			c.m.OpsDone(err)
			if err == nil && result != nil && result.ExitCode == 0 && ValidateActionResult(result) && c.opt.LocalCache != nil {
				if werr := c.opt.LocalCache.SetActionResult(ctx, d, result); werr != nil {
					clog.Warningf(ctx, "failed to write action result with digest %s to local cache: %v", d.String(), werr)
				}
			}
			return result, err
		}
		monitoring.RecordCancellation(ctx, "cache-check", "pre_first_byte")
		retried = true
	}
	start := time.Now()
	result, err := client.GetActionResult(ctx, req)
	if retried {
		monitoring.RecordRetryDuration(ctx, "cache-check", time.Since(start), err)
	}
	c.m.OpsDone(err)
	if err == nil && result != nil && result.ExitCode == 0 && ValidateActionResult(result) && c.opt.LocalCache != nil {
		if werr := c.opt.LocalCache.SetActionResult(ctx, d, result); werr != nil {
			clog.Warningf(ctx, "failed to write action result with digest %s to local cache: %v", d.String(), werr)
		}
	}
	return result, err
}

// SetActionResult sets the action result in local cache if configured.
func (c *Client) SetActionResult(ctx context.Context, d digest.Digest, ar *rpb.ActionResult) error {
	if c.opt.LocalCache != nil {
		return c.opt.LocalCache.SetActionResult(ctx, d, ar)
	}
	return nil
}

// UpdateActionResultEnabled reports whether UpdateActionResult is supported or not.
func (c *Client) UpdateActionResultEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capabilities.GetCacheCapabilities().GetActionCacheUpdateCapabilities().GetUpdateEnabled()
}

// UpdateActionResult updates the action result by the digest.
func (c *Client) UpdateActionResult(ctx context.Context, d digest.Digest, result *rpb.ActionResult) error {
	if !c.APIVersion().useOutputSymlinks() {
		// backend may not support output_symlinks.
		// use output_file_symlinks instead.
		result = proto.CloneOf(result)
		result.OutputFileSymlinks = result.OutputSymlinks //nolint:staticcheck // existing deprecation
		result.OutputSymlinks = nil
	}

	client := rpb.NewActionCacheClient(c.casConn)
	_, err := client.UpdateActionResult(ctx, &rpb.UpdateActionResultRequest{
		InstanceName:   c.opt.Instance,
		ActionDigest:   d.Proto(),
		ActionResult:   result,
		DigestFunction: c.digestFn.Value(),
	})
	c.m.OpsDone(err)
	return err
}

// APIVersion returns the api version negotiated by Init.
func (c *Client) APIVersion() Version {
	if c == nil {
		return Version{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiVersion
}

// Instance returns the instance name.
func (c *Client) Instance() string {
	if c == nil {
		return ""
	}
	return c.opt.Instance
}

// Version is a RE API version.
type Version struct {
	Major, Minor int32
}

// versionOf converts a SemVer to a Version.
func versionOf(v *semverpb.SemVer) Version {
	return Version{Major: v.GetMajor(), Minor: v.GetMinor()}
}

// AtLeast reports whether v is the same as or newer than w.
func (v Version) AtLeast(w Version) bool {
	return v.Major > w.Major || v.Major == w.Major && v.Minor >= w.Minor
}

// String returns v as <major>.<minor>.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// UseActionForPlatformProperties returns true
// when set Platform properties in Action message, as well as Command.
//
//	message Action
//	 // New in version 2.2: clients SHOULD set these platform properties
//	 // as well as those in the Command. Servers SHOULD prefer those set here.
//	 Platform platform
//
//	message Command
//	 // DEPRECATED as of v2.2: platform properties are now specified directly
//	 // in the action.
//	 Platform platform
func (v Version) UseActionForPlatformProperties() bool {
	return v.AtLeast(Version{2, 2})
}

// UseOutputPaths returns true
// when use output_paths instead of output_files, output_directories
// in Command.
//
//	message Command
//	  // DEPRECATED since v2.1: Use `output_paths` instead.
//	  repeated string output_files
//
//	  // DEPRECATED since v2.1: Use `output_paths` instead.
//	  repeated string output_directories
//
//	  // New in v2.1: this fields supersedes the DEPRECATED `output_files`
//	  // and `output_directories` fields.  If `output_paths` is used,
//	  // `output_files` and `output_directories` will be ignored!
//	  repeated string output_paths
func (v Version) UseOutputPaths() bool {
	return v.AtLeast(Version{2, 1})
}

// useOutputSymlinks returns true
// when output_symlinks instead of output_file_symlinks or
// output_directory_symlinks in ActionResult.
//
//	message ActionResult
//	     // DEPRECATED as of v2.1. Servers that wish to be compatible with
//	     // v2.0 API should still populate this field in addition to
//	     // `output_symlinks`.
//	     repeated string output_file_symlinks
//
//	     // DEPRECATED as of v2.1. Servers that wish to be compatible with
//	     // v2.0 API should still populate this field in addition to
//	     // `output_symlinks`.
//	     repeated string output_directory_symlinks
//
//	     // New in v2.1: this field will only be populated if the command
//	     // `output_paths` field was used, and not the pre v2.1
//	     // `output_files` or `output_directories` fields.
//	     repeated string output_symlinks
func (v Version) useOutputSymlinks() bool {
	return v.AtLeast(Version{2, 1})
}

// Range of RE API versions that siso implements.
var (
	minAPIVersion = Version{2, 0}
	maxAPIVersion = Version{2, 3}
)

// negotiateAPIVersion returns the requested api version (v<major>.<minor>),
// or if empty, the highest version that both siso and the server support.
// It fails if that version is outside either range.
// A server that doesn't advertise a version is treated as v2.0.
func negotiateAPIVersion(capa *rpb.ServerCapabilities, requested string) (Version, error) {
	v20 := Version{2, 0}
	serverLow := cmp.Or(versionOf(capa.GetLowApiVersion()), v20)
	serverHigh := cmp.Or(versionOf(capa.GetHighApiVersion()), v20)
	low, high := minAPIVersion, maxAPIVersion
	if serverLow.AtLeast(low) {
		low = serverLow
	}
	if high.AtLeast(serverHigh) {
		high = serverHigh
	}
	ranges := fmt.Sprintf("server supports %s..%s, siso supports %s..%s", serverLow, serverHigh, minAPIVersion, maxAPIVersion)
	if requested == "" {
		if !high.AtLeast(low) {
			return Version{}, fmt.Errorf("no common reapi version: %s", ranges)
		}
		return high, nil
	}
	v, err := parseAPIVersion(requested)
	if err != nil {
		return Version{}, err
	}
	if !v.AtLeast(low) || !high.AtLeast(v) {
		return Version{}, fmt.Errorf("unsupported reapi version %s: %s", requested, ranges)
	}
	return v, nil
}

// parseAPIVersion parses an api version in format of v<major>.<minor>.
func parseAPIVersion(s string) (Version, error) {
	majorStr, minorStr, ok := strings.Cut(strings.TrimPrefix(s, "v"), ".")
	major, majorErr := strconv.ParseUint(majorStr, 10, 31)
	minor, minorErr := strconv.ParseUint(minorStr, 10, 31)
	if !strings.HasPrefix(s, "v") || !ok || majorErr != nil || minorErr != nil {
		return Version{}, fmt.Errorf("invalid reapi version %q: want v<major>.<minor>", s)
	}
	return Version{Major: int32(major), Minor: int32(minor)}, nil
}

// NewContext returns new context with request metadata.
func NewContext(ctx context.Context, rmd *rpb.RequestMetadata) context.Context {
	// Carry the current trace span (if any) as an OpenTelemetry span context so
	// RBE RPCs join that span's trace server-side (e.g. Dapper). Inert without a
	// configured TracerProvider; see Option.TracerProvider.
	if sp := trace.CurSpan(ctx); sp != nil {
		if tid, sid, ok := sp.RawIDs(); ok {
			ctx = oteltrace.ContextWithSpanContext(ctx, oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
				TraceID:    tid,
				SpanID:     sid,
				TraceFlags: oteltrace.FlagsSampled,
			}))
		}
	}
	if rmd == nil {
		rmd = &rpb.RequestMetadata{}
	}
	ver, err := version.Current()
	if err == nil {
		rmd.ToolDetails = &rpb.ToolDetails{
			ToolName:    ver.ToolName(),
			ToolVersion: ver.ToolVersion(),
		}
	}
	// Set metadata on the context, replacing any existing value so that nested
	// NewContext calls don't accumulate multiple entries (servers such as
	// Kajiya reject requests with more than one requestmetadata-bin).
	// See the document for the specification.
	// https://github.com/bazelbuild/remote-apis/blob/8f539af4b407a4f649707f9632fc2b715c9aa065/build/bazel/remote/execution/v2/remote_execution.proto#L2034-L2045
	b, err := proto.Marshal(rmd)
	if err != nil {
		clog.Warningf(ctx, "marshal %v: %v", rmd, err)
		return ctx
	}
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.MD{}
	} else {
		md = md.Copy()
	}
	md.Set(requestMetadataKey, string(b))
	return metadata.NewOutgoingContext(ctx, md)
}

// MetadataFromOutgoingContext returns request metadata in outgoing context.
func MetadataFromOutgoingContext(ctx context.Context) (*rpb.RequestMetadata, bool) {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return nil, false
	}
	v, ok := md[requestMetadataKey]
	if !ok {
		return nil, false
	}
	if len(v) == 0 {
		return nil, false
	}
	rmd := &rpb.RequestMetadata{}
	err := proto.Unmarshal([]byte(v[0]), rmd)
	if err != nil {
		return nil, false
	}
	return rmd, true
}

// MaxRetries returns the configured maximum number of retries.
func (c *Client) MaxRetries() int {
	return c.opt.MaxRetries
}
