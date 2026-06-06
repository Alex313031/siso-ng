// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
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
	otelPort := "4317"

	// 1. Start collector
	baseURL := startCollector(t, sisoBin, "localhost:"+otelPort, otelPort)

	// 2. Verify Status
	t.Run("Status", func(t *testing.T) {
		verifyStatus(t, baseURL)
	})

	// 3. Verify Config
	t.Run("Config", func(t *testing.T) {
		verifyConfig(t, baseURL, "localhost:4317", "tcp")
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

func startCollector(t *testing.T, sisoBin, collectorAddr string, extraPorts ...string) string {
	t.Helper()
	healthPort := "13133"
	killProcessOnPort(t, healthPort)
	killProcessOnPort(t, "15154")
	for _, p := range extraPorts {
		killProcessOnPort(t, p)
	}

	cmd := exec.Command(sisoBin, "collector",
		"-project=test",
		"-collector_address="+collectorAddr,
		"-insecure",
	)
	cmd.Env = append(os.Environ(), "SISO_CREDENTIAL_HELPER=mTLS")

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

	baseURL := "http://localhost:" + healthPort
	if err := waitForHealth(baseURL+"/health/status", 10*time.Second); err != nil {
		t.Fatalf("collector failed to become healthy: %v", err)
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

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}

	// Navigate to receivers.otlp.protocols.grpc
	receivers, ok := got["receivers"].(map[string]any)
	if !ok {
		t.Fatalf("config missing 'receivers'")
	}
	otlp, ok := receivers["otlp"].(map[string]any)
	if !ok {
		t.Fatalf("config missing 'otlp'")
	}
	protocols, ok := otlp["protocols"].(map[string]any)
	if !ok {
		t.Fatalf("config missing 'protocols'")
	}
	grpcCfg, ok := protocols["grpc"].(map[string]any)
	if !ok {
		t.Fatalf("config missing 'grpc'")
	}

	endpoint, _ := grpcCfg["endpoint"].(string)
	transport, _ := grpcCfg["transport"].(string)

	if endpoint != expectedEndpoint {
		t.Errorf("expected endpoint %q, got %q", expectedEndpoint, endpoint)
	}
	if transport != expectedTransport {
		t.Errorf("expected transport %q, got %q", expectedTransport, transport)
	}

	// Scrub to match golden
	// Golden expects localhost:4317 and tcp
	grpcCfg["endpoint"] = "localhost:4317"
	grpcCfg["transport"] = "tcp"

	goldenPath := filepath.Join("testdata", "health_config.json")
	checkGolden(t, got, goldenPath)
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

func killProcessOnPort(t *testing.T, port string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("netstat", "-aon").Output()
		if err != nil {
			t.Logf("netstat failed: %v", err)
			return
		}
		lines := strings.Split(string(out), "\n")
		target := "127.0.0.1:" + port

		for _, line := range lines {
			parts := strings.Fields(line)
			if len(parts) < 5 {
				continue
			}
			// Proto Local Address Foreign Address State PID
			// TCP    127.0.0.1:13133   0.0.0.0:0       LISTENING     34228
			// parts[1] is local address.
			if parts[1] == target {
				// PID is the last element.
				pid := parts[len(parts)-1]
				if pid != "0" {
					// Kill the very first non 0 process we find.
					err := exec.Command("taskkill", "/F", "/T", "/PID", pid).Run()
					if err != nil {
						t.Logf("Failed to kill PID %s: %v", pid, err)
					} else {
						t.Logf("Killed PID %s on port %s", pid, port)
					}
					return
				}
			}
		}
		return
	}
	// lsof -t -i:PORT returns PIDs.
	out, err := exec.Command("lsof", "-t", "-i:"+port).Output()
	if err != nil {
		// Likely no process found (exit code 1).
		return
	}
	pidStr := strings.Fields(string(out))[0]

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Kill(); err != nil {
		t.Logf("Failed to kill PID %d: %v", pid, err)
	} else {
		t.Logf("Killed PID %d on port %s", pid, port)
	}
}
