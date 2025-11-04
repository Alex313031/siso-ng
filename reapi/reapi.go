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

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	semverpb "github.com/bazelbuild/remote-apis/build/bazel/semver"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/version"
)

// Option contains options of remote exec API.
type Option struct {
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
	return envs
}

// RegisterFlags registers flags on the option.
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
	fs.StringVar(&o.CASAddress, o.Prefix+"_cas_address", "", "reapi cas address"+purpose+" (if empty, share conn with "+o.Prefix+"_address)")
	instance, ok := envs["SISO_REAPI_INSTANCE"]
	if !ok {
		instance = "default_instance"
	}
	fs.StringVar(&o.Instance, o.Prefix+"_instance", instance, "reapi instance name"+purpose)

	fs.BoolVar(&o.Insecure, o.Prefix+"_insecure", os.Getenv("RBE_service_no_security") == "true", "reapi insecure mode. default can be set by $RBE_service_no_security")

	fs.StringVar(&o.TLSClientAuthCert, o.Prefix+"_tls_client_auth_cert", os.Getenv("RBE_tls_client_auth_cert"), "Certificate to use when using mTLS to connect to the RE api service. default can be set by $RBE_tls_client_auth_cert")
	fs.StringVar(&o.TLSClientAuthKey, o.Prefix+"_tls_client_auth_key", os.Getenv("RBE_tls_client_auth_key"), "Key to use when using mTLS to connect to the RE api service. default can be set by $RBE_tls_client_auth_key")

	fs.StringVar(&o.TLSCACert, o.Prefix+"_tls_ca_cert", os.Getenv("RBE_tls_ca_cert"), "Load TLS CA certificates from this file to connect to the RE api service. default can be set by $RBE_tls_ca_cert")

	fs.Int64Var(&o.CompressedBlob, o.Prefix+"_compress_blob", 1024, "use compressed blobs if server supports compressed blobs and size is bigger than this. specify 0 to disable blob-level compression."+purpose)

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
	conn    grpcClientConn
	casConn grpcClientConn

	capabilities *rpb.ServerCapabilities
	apiVersion   *semverpb.SemVer

	knownDigests sync.Map // key:digest.Digest, value: *uploadOp or true

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
	return NewFromConn(ctx, opt, conn, casConn)
}

func newConn(ctx context.Context, addr string, cred cred.Cred, opt Option) (grpcClientConn, error) {
	copts := []option.ClientOption{
		option.WithEndpoint(addr),
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
func NewFromConn(ctx context.Context, opt Option, conn, casConn grpcClientConn) (*Client, error) {
	cc := rpb.NewCapabilitiesClient(conn)
	capa, err := cc.GetCapabilities(ctx, &rpb.GetCapabilitiesRequest{
		InstanceName: opt.Instance,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	clog.Infof(ctx, "capabilities of %s: %s", opt.Instance, capa)
	if opt.CompressedBlob > 0 {
		opt.compressor = selectCompressor(capa.GetCacheCapabilities().GetSupportedCompressors())
		if opt.compressor != rpb.Compressor_IDENTITY {
			clog.Infof(ctx, "compressed-blobs/%s for > %d", strings.ToLower(opt.compressor.String()), opt.CompressedBlob)
		} else {
			clog.Infof(ctx, "compressed-blobs is not supported")
		}
	}
	var apiVersion *semverpb.SemVer
	if opt.REAPIVersion != "" {
		var major, minor int32
		_, err := fmt.Sscanf(opt.REAPIVersion, "v%d.%d", &major, &minor)
		if err != nil {
			clog.Warningf(ctx, "failed to parse reapi version %q: %v", opt.REAPIVersion, err)
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
	c := &Client{
		opt:          opt,
		conn:         conn,
		casConn:      casConn,
		capabilities: capa,
		apiVersion:   apiVersion,
		m:            iometrics.New("reapi"),
	}
	c.knownDigests.Store(digest.Empty, true)
	return c, nil
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
	if c.apiVersion != nil {
		return c.apiVersion
	}
	return c.capabilities.GetHighApiVersion()
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
	ver, err := version.Current()
	if err == nil {
		rmd.ToolDetails = &rpb.ToolDetails{
			ToolName:    ver.ToolName(),
			ToolVersion: ver.ToolVersion(),
		}
	}
	// Append metadata to the context.
	// See the document for the specification.
	// https://github.com/bazelbuild/remote-apis/blob/8f539af4b407a4f649707f9632fc2b715c9aa065/build/bazel/remote/execution/v2/remote_execution.proto#L2034-L2045
	b, err := proto.Marshal(rmd)
	if err != nil {
		clog.Warningf(ctx, "marshal %v: %v", rmd, err)
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx,
		"build.bazel.remote.execution.v2.requestmetadata-bin",
		string(b))
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
