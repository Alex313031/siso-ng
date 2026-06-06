// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package reapi provides remote execution API.
package reapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"
	semverpb "go.chromium.org/build/remote-apis/build/bazel/semver"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/retry"
	"go.chromium.org/build/siso/version"
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
	// compressor for ByteStream Read/Write APIs.
	compressor rpb.Compressor_Value
	// Threshold that decides whether to use ByteStream API (compression-aware)
	// instead of BatchReadBlobs if blob size is bigger than this.
	ByteStreamReadThreshold int64

	// Enables GRPC compression. If enabled, blob-level compression will be
	// forcibly disabled.
	EnableGRPCCompression bool

	// Keep Execute stream open as lone as possible.
	// If false, siso closes Execute stream every 1 minute and retries
	// with WaitExecution to mitigate grpc/network issue.
	KeepExecStream bool

	ConnPool        int
	KeepAliveParams keepalive.ClientParameters

	// RE API version to use by siso, in format of v<major>.<minor>
	// e.g. "v2.0".
	// default to use high api version advertised by the server
	// capabilities.
	REAPIVersion string

	// UploadConcurrency caps in-flight upload RPCs per UploadAll call.
	// Zero (default) means serial; set to max(32, GOMAXPROCS*4) or similar
	// for callers that benefit from parallel upload (e.g. `siso isolate`).
	UploadConcurrency int
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

	fs.Int64Var(&o.ByteStreamReadThreshold, o.Prefix+"_byte_stream_read_threshold", 2*1024*1024, "if blob size >= threshold, use ByteStream API (compression-aware)"+purpose)

	fs.BoolVar(&o.EnableGRPCCompression, o.Prefix+"_enable_grpc_compression", false, "enable grpc compression.  if enabled, blob-level compression will be forcibly disabled."+purpose)

	fs.BoolVar(&o.KeepExecStream, o.Prefix+"_keep_exec_stream", false, "keep Execute stream open as long as possible")

	fs.IntVar(&o.ConnPool, o.Prefix+"_grpc_conn_pool", 25, "grpc connection pool")

	// https://grpc.io/docs/guides/keepalive/#keepalive-configuration-specification
	// b/286237547 - RBE suggests 30s
	fs.DurationVar(&o.KeepAliveParams.Time, o.Prefix+"_grpc_keepalive_time", 30*time.Second, "grpc keepalive time"+purpose)
	fs.DurationVar(&o.KeepAliveParams.Timeout, o.Prefix+"_grpc_keepalive_timeout", 20*time.Second, "grpc keepalive timeout"+purpose)
	fs.BoolVar(&o.KeepAliveParams.PermitWithoutStream, o.Prefix+"_grpc_keepalive_permit_without_stream", false, "grpc keepalive permit without stream"+purpose)

	fs.StringVar(&o.REAPIVersion, o.Prefix+"_version_to_use", "", "specify re api version to use, in format of v<major>.<minor>. e.g. v2.0")

	// Flags only supported for "execution".
	if o.Prefix == "reapi" {
		fs.IntVar(&o.ExecutionPriority, o.Prefix+"_priority", 0, "reapi priority for action executions"+purpose+". The semantics and supported values depend on the backend")
	}
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
// Just returns empty string if backend is not RBE.
func (o *Option) UpdateProjectID(projID string) string {
	if !isGoogleRBE(o.Address) {
		return ""
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
	opt     Option
	cred    cred.Cred
	conn    grpcClientConn
	casConn grpcClientConn

	mu           sync.Mutex
	capabilities *rpb.ServerCapabilities
	apiVersion   *semverpb.SemVer

	knownDigests sync.Map // key:digest.Digest, value: *uploadOp or true

	zstdDecoderPool *sync.Pool

	m *iometrics.IOMetrics
}

// serviceConfig is gRPC service config for RE API.
// https://github.com/bazelbuild/bazel/blob/7.1.1/src/main/java/com/google/devtools/build/lib/remote/RemoteRetrier.java#L47
var serviceConfig = `
{
	"loadBalancingConfig": [{"round_robin":{}}],
	"methodConfig": [
	  {
		"name": [
                  { "service": "build.bazel.remote.execution.v2.Execution" }
                ],
		"timeout": "600s",
		"retryPolicy": {
			"maxAttempts": 5,
			"initialBackoff": "1s",
			"maxBackoff": "120s",
			"backoffMultiplier": 1.6,
			"retryableStatusCodes": [
				"ABORTED",
				"INTERNAL",
				"RESOURCE_EXHAUSTED",
				"UNAVAILABLE",
				"UNKNOWN"
			]
		}
	  },
          {
		"name": [
                  {
                    "service": "build.bazel.remote.execution.v2.ActionCache",
                    "method": "GetActionResult"
                  }
                ],
		"timeout": "10s",
		"retryPolicy": {
			"maxAttempts": 5,
			"initialBackoff": "0.1s",
			"maxBackoff": "1s",
			"backoffMultiplier": 1.6,
			"retryableStatusCodes": [
				"ABORTED",
				"INTERNAL",
				"RESOURCE_EXHAUSTED",
				"UNAVAILABLE",
				"UNKNOWN"
			]
		}
          },
	  {
		"name": [
                  { "service": "build.bazel.remote.execution.v2.ActionCache" },
                  { "service": "build.bazel.remote.execution.v2.ContentAddressableStorage" },
                  { "service": "build.bazel.remote.execution.v2.Capabilities" }
                ],
		"timeout": "300s",
		"retryPolicy": {
			"maxAttempts": 5,
			"initialBackoff": "0.1s",
			"maxBackoff": "1s",
			"backoffMultiplier": 1.6,
			"retryableStatusCodes": [
				"ABORTED",
				"INTERNAL",
				"RESOURCE_EXHAUSTED",
				"UNAVAILABLE",
				"UNKNOWN"
			]
		}
	  }
        ]
}`

func DialOptions(keepAliveParams keepalive.ClientParameters) []grpc.DialOption {
	// TODO(b/273639326): handle auth failures gracefully.

	// https://github.com/grpc/grpc/blob/c16338581dba2b054bf52484266b79e6934bbc1c/doc/service_config.md
	// https://github.com/grpc/proposal/blob/9f993b522267ed297fe54c9ee32cfc13699166c7/A6-client-retries.md
	// timeout=300s may cause deadline exceeded to fetch large *.so file?
	dopts := append([]grpc.DialOption(nil),
		grpc.WithKeepaliveParams(keepAliveParams),
		grpc.WithDisableServiceConfig(),
		// no retry for ActionCache
		grpc.WithDefaultServiceConfig(serviceConfig),
	)
	return dopts
}

// New creates new remote exec API client.
func New(ctx context.Context, cred cred.Cred, opt Option) (*Client, error) {
	defer trace.Begin(ctx, "reapi.New").End()
	if opt.Address == "" {
		return nil, errors.New("no reapi address")
	}
	if isGoogleRBE(opt.Address) && opt.Instance == "" {
		return nil, errors.New("no reapi instance")
	}
	clog.Infof(ctx, "address: %q instance: %q", opt.Address, opt.Instance)
	conn, err := newConn(ctx, opt.Address, cred, opt)
	if err != nil {
		return nil, err
	}
	casConn := conn
	if opt.CASAddress != "" {
		clog.Infof(ctx, "cas address: %q", opt.CASAddress)
		casConn, err = newConn(ctx, opt.CASAddress, cred, opt)
		if err != nil {
			conn.Close()
			return nil, err
		}
	}
	return NewFromConn(ctx, opt, cred, conn, casConn)
}

func newConn(ctx context.Context, addr string, cred cred.Cred, opt Option) (grpcClientConn, error) {
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
		option.WithGRPCConnectionPool(opt.ConnPool),
	}
	if !isGoogleRBE(addr) {
		// disable Google Application Default for non RBE backend.
		// user should specify credential helper for the backend.
		copts = append(copts, option.WithoutAuthentication())
	}
	dopts := DialOptions(opt.KeepAliveParams)
	if opt.EnableGRPCCompression {
		dopts = append(dopts, grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)))
		if opt.CompressedBlob != 0 {
			opt.CompressedBlob = 0
			clog.Warningf(ctx, "disabling blob compression because grpc compression is enabled")
		}
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

// NewFromConn creates new remote exec API client from conn and casConn.
func NewFromConn(ctx context.Context, opt Option, cred cred.Cred, conn, casConn grpcClientConn) (*Client, error) {
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
	c := &Client{
		opt:             opt,
		cred:            cred,
		conn:            conn,
		casConn:         casConn,
		zstdDecoderPool: zstdDecoderPool,
		m:               iometrics.New("reapi"),
	}
	c.knownDigests.Store(digest.Empty, true)
	return c, nil
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
		c.conn.Close()
		return fmt.Errorf("failed to get capabilities: %w", err)
	}
	clog.Infof(ctx, "capabilities of %s: %s", c.opt.Instance, capa)
	if c.opt.CompressedBlob > 0 {
		c.opt.compressor = selectCompressor(capa.GetCacheCapabilities().GetSupportedCompressors())
		if c.opt.compressor != rpb.Compressor_IDENTITY {
			clog.Infof(ctx, "compressed-blobs/%s for > %d", strings.ToLower(c.opt.compressor.String()), c.opt.CompressedBlob)
		} else {
			clog.Infof(ctx, "compressed-blobs is not supported")
		}
	}
	clog.Infof(ctx, "byte stream read threshold: %d", c.opt.ByteStreamReadThreshold)
	var apiVersion *semverpb.SemVer
	if c.opt.REAPIVersion != "" {
		var major, minor int32
		_, err := fmt.Sscanf(c.opt.REAPIVersion, "v%d.%d", &major, &minor)
		if err != nil {
			clog.Warningf(ctx, "failed to parse reapi version %q: %v", c.opt.REAPIVersion, err)
		} else {
			apiVersion = &semverpb.SemVer{
				Major: major,
				Minor: minor,
			}
			highVer := capa.GetHighApiVersion()
			if highVer.GetMajor() < major || (highVer.GetMajor() == major && highVer.GetMinor() < minor) {
				clog.Errorf(ctx, "higher api version is specified than server capabilities: %v > %v", apiVersion, highVer)
			}
			lowVer := capa.GetLowApiVersion()
			if lowVer.GetMajor() > major || (lowVer.GetMajor() == major && lowVer.GetMinor() > minor) {
				clog.Errorf(ctx, "lower api version is specified than server capabilities: %v < %v", apiVersion, lowVer)
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capabilities = capa
	c.apiVersion = apiVersion
	return nil
}

// Close closes the client.
func (c *Client) Close() error {
	return c.conn.Close()
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
	b, err := c.Get(ctx, d, fmt.Sprintf("%s -> %T", d, p))
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, p)
}

// GetActionResult gets the action result by the digest.
func (c *Client) GetActionResult(ctx context.Context, d digest.Digest) (*rpb.ActionResult, error) {
	client := rpb.NewActionCacheClient(c.casConn)
	result, err := client.GetActionResult(ctx, &rpb.GetActionResultRequest{
		InstanceName: c.opt.Instance,
		ActionDigest: d.Proto(),
	})
	c.m.OpsDone(err)
	return result, err
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
	client := rpb.NewActionCacheClient(c.casConn)
	_, err := client.UpdateActionResult(ctx, &rpb.UpdateActionResultRequest{
		InstanceName: c.opt.Instance,
		ActionDigest: d.Proto(),
		ActionResult: result,
	})
	c.m.OpsDone(err)
	return err
}

// APIVersion returns api version to use.
func (c *Client) APIVersion() *semverpb.SemVer {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.apiVersion != nil {
		return c.apiVersion
	}
	return c.capabilities.GetHighApiVersion()
}

// Instance returns the instance name.
func (c *Client) Instance() string {
	if c == nil {
		return ""
	}
	return c.opt.Instance
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
func UseActionForPlatformProperties(apiVer *semverpb.SemVer) bool {
	return apiVer.GetMajor() >= 2 && apiVer.GetMinor() >= 2
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
func UseOutputPaths(apiVer *semverpb.SemVer) bool {
	return apiVer.GetMajor() >= 2 && apiVer.GetMinor() >= 1
}

// NewContext returns new context with request metadata.
func NewContext(ctx context.Context, rmd *rpb.RequestMetadata) context.Context {
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
	md.Set("build.bazel.remote.execution.v2.requestmetadata-bin", string(b))
	return metadata.NewOutgoingContext(ctx, md)
}

// MetadataFromOutgoingContext returns request metadata in outgoing context.
func MetadataFromOutgoingContext(ctx context.Context) (*rpb.RequestMetadata, bool) {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return nil, false
	}
	v, ok := md["build.bazel.remote.execution.v2.requestmetadata-bin"]
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
