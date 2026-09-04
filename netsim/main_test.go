package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Runs always land in the same place regardless of where the command was invoked, so a run is
// never left somewhere arbitrary.
func TestRunsLandInTheRepository(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skip("not in a repository")
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("repoRoot returned %s, which is not the repository", root)
	}
	wd, _ := os.Getwd()
	if !strings.HasPrefix(wd, root) {
		t.Errorf("the working directory %s is outside the repository root %s", wd, root)
	}
}

// The artifacts a run writes are inside the repository and ignored by git, so they are always
// found in the same place and never committed.
func TestRunArtifactsAreIgnoredByGit(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skip("not in a repository")
	}
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "/netsim-runs/") {
		t.Error("netsim-runs/ must be ignored: a run writes databases, logs and keys into it")
	}
}
