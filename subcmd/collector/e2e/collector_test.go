// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// siso-owned settings the rendered /health/config must report back. testProject
// is supplied to the collector at launch; the rest mirror config.yaml.
const (
	testProject    = "test"
	metricPrefix   = "workload.googleapis.com/siso"
	defaultLogName = "opentelemetry.io/collector-exported-log"
	otlpReceiver   = "otlp"
	gceExporter    = "googlecloud"
)

// e2e test takes around 5 second to complete.
func TestCollector(t *testing.T) {
	// 1. Build siso binary once for all tests
	sisoBin := buildSiso(t)

	t.Run("TCP", func(t *testing.T) {
		testCollectorTCP(t, sisoBin)
	})

	t.Run("UnixSocket", func(t *testing.T) {
		testCollectorUnix(t, sisoBin)
	})
}

func buildSiso(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()
	sisoBin := filepath.Join(tempDir, "siso")
	if runtime.GOOS == "windows" {
		sisoBin += ".exe"
	}

	buildCmd := exec.Command("go", "build", "-o", sisoBin, "go.chromium.org/build/siso")
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build siso: %v\n%s", err, out)
	}
	return sisoBin
}

func testCollectorTCP(t *testing.T, sisoBin string) {
	// Bind the otlp receiver to a free port so concurrent runs don't collide.
	otlpAddr := freeAddr(t)

	// 1. Start collector
	baseURL := startCollector(t, sisoBin, otlpAddr)

	// 2. Verify Status
	t.Run("Status", func(t *testing.T) {
		verifyStatus(t, baseURL)
	})

	// 3. Verify Config
	t.Run("Config", func(t *testing.T) {
		verifyConfig(t, baseURL, otlpAddr, "tcp")
	})
}

func testCollectorUnix(t *testing.T, sisoBin string) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not tested on Windows")
	}

	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "siso.sock")

	// Handle potential long path on Darwin/Linux
	if len(socketPath) >= 100 {
		// Use a shorter temporary path in /tmp to avoid socket path length limits
		tempDir = tempDir[:90]
		socketPath = filepath.Join(tempDir, "siso.sock")
	}

	// 1. Confirm socket file is not there before start
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket file %s should not exist before start", socketPath)
	}

	// 2. Start collector
	baseURL := startCollector(t, sisoBin, "unix://"+socketPath)

	// 3. Confirm socket file is now there
	if err := waitForFile(socketPath, 5*time.Second); err != nil {
		t.Fatalf("socket file %s did not appear: %v", socketPath, err)
	}

	// 4. Verify Status
	t.Run("Status", func(t *testing.T) {
		verifyStatus(t, baseURL)
	})

	// 5. Verify Config
	t.Run("Config", func(t *testing.T) {
		verifyConfig(t, baseURL, socketPath, "unix")
	})
}

// startCollector launches "siso collector" with its otlp receiver on otlpAddr
// and returns the health-check HTTP base URL once the collector is healthy.
func startCollector(t *testing.T, sisoBin, otlpAddr string) string {
	t.Helper()

	// Pick free ports for the health and internal-metrics listeners. Their
	// production defaults (13133/13132/15154) are fixed, so without overrides
	// a second collector on the same host fails to bind and never reports
	// healthy. Allocate them in one batch so they're guaranteed distinct.
	ports := freePorts(t, 3)
	healthAddr := loopbackAddr(ports[0])
	healthGRPC := loopbackAddr(ports[1])
	metricsPort := ports[2]

	cmd := exec.Command(sisoBin, "collector",
		"-project="+testProject,
		"-collector_address="+otlpAddr,
		"-insecure",
	)
	cmd.Env = append(os.Environ(),
		"SISO_CREDENTIAL_HELPER=mTLS",
		"SISO_COLLECTOR_HEALTH_ENDPOINT="+healthAddr,
		"SISO_COLLECTOR_HEALTH_GRPC_ENDPOINT="+healthGRPC,
		"SISO_COLLECTOR_METRICS_PORT="+strconv.Itoa(metricsPort),
	)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start collector: %v", err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("collector output:\n%s", buf.String())
		}
	})

	baseURL := "http://" + healthAddr
	if err := waitForHealth(baseURL+"/health/status", 10*time.Second); err != nil {
		t.Fatalf("collector failed to become healthy: %v\noutput:\n%s", err, buf.String())
	}
	return baseURL
}

func verifyStatus(t *testing.T, baseURL string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/health/status")
	if err != nil {
		t.Fatalf("failed to get status: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}

	// Scrub timestamps
	got["start_time"] = "0000-00-00T00:00:00Z"
	got["status_time"] = "0000-00-00T00:00:00Z"

	goldenPath := filepath.Join("testdata", "health_status.json")
	checkGolden(t, got, goldenPath)
}

// verifyConfig checks the fields of the rendered collector config that siso
// actually controls: the otlp receiver wired from -collector_address, the
// googlecloud exporter settings from config.yaml, and the telemetry pipelines.
// The rest of the rendered config is upstream collector defaults and is left
// unchecked so a dependency bump can't break this test for no real reason.
func verifyConfig(t *testing.T, baseURL, expectedEndpoint, expectedTransport string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/health/config")
	if err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}

	var cfg struct {
		Receivers struct {
			OTLP struct {
				Protocols struct {
					GRPC struct {
						Endpoint  string `json:"endpoint"`
						Transport string `json:"transport"`
					} `json:"grpc"`
				} `json:"protocols"`
			} `json:"otlp"`
		} `json:"receivers"`
		Exporters struct {
			GoogleCloud struct {
				Project string `json:"project"`
				Log     struct {
					DefaultLogName string `json:"default_log_name"`
				} `json:"log"`
				Metric struct {
					Prefix string `json:"prefix"`
				} `json:"metric"`
			} `json:"googlecloud"`
		} `json:"exporters"`
		Service struct {
			Pipelines map[string]struct {
				Receivers []string `json:"receivers"`
				Exporters []string `json:"exporters"`
			} `json:"pipelines"`
		} `json:"service"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}

	grpc := cfg.Receivers.OTLP.Protocols.GRPC
	if grpc.Endpoint != expectedEndpoint {
		t.Errorf("otlp endpoint = %q, want %q", grpc.Endpoint, expectedEndpoint)
	}
	if grpc.Transport != expectedTransport {
		t.Errorf("otlp transport = %q, want %q", grpc.Transport, expectedTransport)
	}

	gce := cfg.Exporters.GoogleCloud
	if gce.Project != testProject {
		t.Errorf("googlecloud project = %q, want %q", gce.Project, testProject)
	}
	if gce.Metric.Prefix != metricPrefix {
		t.Errorf("googlecloud metric prefix = %q, want %q", gce.Metric.Prefix, metricPrefix)
	}
	if gce.Log.DefaultLogName != defaultLogName {
		t.Errorf("googlecloud log default_log_name = %q, want %q", gce.Log.DefaultLogName, defaultLogName)
	}

	// Every telemetry signal must flow otlp -> googlecloud.
	for _, signal := range []string{"traces", "metrics", "logs"} {
		p, ok := cfg.Service.Pipelines[signal]
		if !ok {
			t.Errorf("missing %q pipeline", signal)
			continue
		}
		if diff := cmp.Diff([]string{otlpReceiver}, p.Receivers); diff != "" {
			t.Errorf("%q pipeline receivers mismatch (-want +got):\n%s", signal, diff)
		}
		if diff := cmp.Diff([]string{gceExporter}, p.Exporters); diff != "" {
			t.Errorf("%q pipeline exporters mismatch (-want +got):\n%s", signal, diff)
		}
	}
}

func loopbackAddr(port int) string {
	return "127.0.0.1:" + strconv.Itoa(port)
}

// freeAddr returns a "127.0.0.1:<port>" loopback address whose port is free.
func freeAddr(t *testing.T) string {
	t.Helper()
	return loopbackAddr(freePorts(t, 1)[0])
}

// freePorts reserves n distinct free loopback TCP ports and returns them. Every
// listener is held open until all n are chosen, so the kernel cannot hand the
// same port out twice; they are then closed for the collector to bind. A port
// could still be taken in the gap before the collector binds it, but ephemeral
// ports make that vanishingly unlikely, unlike the fixed ports the daemon
// defaults to. Only TCP is probed because every collector listener is TCP.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to allocate free port: %v", err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		l.Close()
	}
	return ports
}

func waitForHealth(url string, timeout time.Duration) error {
	start := time.Now()
	for time.Since(start) < timeout {
		if isPortOpen(url) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout reaching %s", url)
}

func isPortOpen(url string) bool {
	resp, err := http.Get(url)
	if err == nil && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		return true
	}
	if resp != nil {
		resp.Body.Close()
	}
	return false
}

func waitForFile(path string, timeout time.Duration) error {
	start := time.Now()
	for time.Since(start) < timeout {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for file %s", path)
}

func checkGolden(t *testing.T, got any, goldenPath string) {
	t.Helper()
	wantBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("failed to read golden file %s: %v", goldenPath, err)
	}

	var want any
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatalf("failed to unmarshal golden file: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Mismatch (-want +got):\n%s", diff)
	}
}
