package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
	"github.com/spf13/cobra"
)

// testEnv holds a temporary database and kernel for CLI tests.
type testEnv struct {
	db  *store.DB
	k   *kernel.Kernel
	dir string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "cli-test-secret"
	k := kernel.New(db, nil, nil, nil, nil, cfg, log.Discard())
	t.Setenv("JUICE_SECRET_KEY", "cli-test-secret")

	t.Cleanup(func() { db.Close() })

	origDB := flagDB
	flagDB = dbPath
	t.Cleanup(func() { flagDB = origDB })

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	return &testEnv{db: db, k: k, dir: dir}
}

func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}
