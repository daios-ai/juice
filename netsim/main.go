package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	rail := flag.String("rail", "play", "which world to run in: play, anvil or sepolia")
	rounds := flag.Int("rounds", 12, "how many times the trading rounds repeat")
	binary := flag.String("juice", "", "the juice binary to drive (built if not given)")
	flag.Parse()

	if err := run(*rail, *rounds, *binary); err != nil {
		fmt.Fprintln(os.Stderr, "netsim: "+err.Error())
		os.Exit(1)
	}
}

func run(railName string, rounds int, binary string) error {
	repo, err := repoRoot()
	if err != nil {
		return err
	}
	if err := os.Chdir(repo); err != nil {
		return err
	}
	if binary == "" {
		binary = filepath.Join(os.TempDir(), "juice-netsim")
		fmt.Println("building the binary under test")
		build := exec.Command("go", "build", "-o", binary, "./cmd/juice/")
		build.Stdout, build.Stderr = os.Stdout, os.Stderr
		if err := build.Run(); err != nil {
			return fmt.Errorf("could not build the binary: %w", err)
		}
	}
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("no binary at %s", binary)
	}

	r, err := NewRail(railName)
	if err != nil {
		return err
	}
	root := filepath.Join(repo, "netsim-runs", railName+"-"+time.Now().Format("20060102-150405"))
	n, err := NewNet(root, binary, r)
	if err != nil {
		return err
	}
	defer n.Close()

	fmt.Printf("netsim: rail=%s rounds=%d\n", railName, rounds)
	fmt.Printf("output: %s\n", root)

	// Whatever happens, the kernels and the chain are stopped: a run that leaves servers behind
	// makes the next one fail for a reason that has nothing to do with the code.
	stop := func() {
		for _, k := range n.Kernels {
			k.Stop()
		}
		_, _ = r.Finish(n)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; fmt.Println("\ninterrupted; stopping everything"); stop(); os.Exit(130) }()

	// The rail is told what the story will ask of it before anything starts, so a rail with a
	// spending limit refuses in advance with numbers rather than running dry halfway through.
	if err := r.Prepare(n, StoryShape()); err != nil {
		stop()
		return err
	}
	backend, srv, err := startBackend(root)
	if err != nil {
		stop()
		return err
	}
	n.Backend = backend
	defer func() { _ = srv.Close() }()

	st, storyErr := Run(n, rounds)
	if storyErr != nil {
		fmt.Fprintln(os.Stderr, "the story stopped early: "+storyErr.Error())
	}

	n.Scenario("collecting the final state")
	cost, costErr := r.Finish(n)
	if costErr != nil {
		fmt.Fprintln(os.Stderr, "rail: "+costErr.Error())
	}
	rep, verdictErr := Judge(n, st, rounds, cost)
	for _, k := range n.Kernels {
		k.Stop()
	}

	fmt.Println()
	fmt.Printf("  report:  %s/report.md\n", root)
	fmt.Printf("  metrics: %s/metrics.json\n", root)
	fmt.Printf("  log:     %s/log.jsonl (%d operations)\n", root, rep.Operations)
	if verdictErr != nil {
		fmt.Printf("  RESULT: %s\n", verdictErr)
		return verdictErr
	}
	if storyErr != nil {
		return storyErr
	}
	fmt.Println("  RESULT: PASS")
	return nil
}

// repoRoot finds the repository regardless of where the command was invoked, so runs always land
// in the same place.
func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("netsim must be run inside the repository")
	}
	return strings.TrimSpace(string(out)), nil
}
