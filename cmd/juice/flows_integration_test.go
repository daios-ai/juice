//go:build integration

package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestFlowsIntegration builds the juice binary and runs scripts/flows_test.sh.
// It covers all 34 CLI user-story flows end-to-end against a real SQLite database.
func TestFlowsIntegration(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "juice")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// Locate the script relative to the module root (two levels up from cmd/juice).
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(moduleRoot, "scripts", "flows_test.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("flows_test.sh not found at %s: %v", script, err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "JUICE="+bin, "JUICE_SECRET_KEY=flows-test-secret")
	cmd.Dir = moduleRoot

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		t.Log(scanner.Text())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("flows test failed: %v", err)
	}
}
