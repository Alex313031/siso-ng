package runtimex_test

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"go.chromium.org/build/siso/runtimex"
)

func TestNumCPU(t *testing.T) {
	n := runtimex.NumCPU()

	// Get expectation from Python's os.cpu_count().
	cmd := exec.Command("python3", "-c", "import os; print(os.cpu_count())")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to run command: %v", err)
	}
	want, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("failed to convert %s to int", out)
	}

	if n != want {
		t.Errorf("NumCPU()=%d, want %d", n, want)
	}
}
