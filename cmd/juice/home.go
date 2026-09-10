package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/daios-ai/juice/kernel"
)

// kernelName is the nickname of the kernel this process serves, given positionally to `serve`: what
// it calls itself on the network (D15) and, being the one name chosen by the time a home is created,
// that home's directory name. The directory may be renamed without the network noticing.
var kernelName string

// validateLocalName accepts the labels this installation may turn into one file or directory name:
// a kernel under kernels/, a context under client/. The rules are the filesystem's, not the
// kernel's — a local label has no relation to a handle, so it borrows no validator from the account
// namespace — and they are one rule rather than two because both end up as a path segment, where a
// name free to hold a separator or a dot could address something other than its own.
// kind names the thing in the message, so an operator is told which label was refused.
func validateLocalName(kind, name string) error {
	if name == "" {
		return kernel.ErrInvalidInput.Wrapf("%s name is required", kind)
	}
	if len(name) > 64 {
		return kernel.ErrInvalidInput.Wrapf("%s name must be at most 64 characters", kind)
	}
	if strings.HasPrefix(name, ".") {
		return kernel.ErrInvalidInput.Wrapf("%s name must not begin with a dot", kind)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return kernel.ErrInvalidInput.Wrapf("%s name may hold only letters, digits, dot, dash and underscore", kind)
		}
	}
	return nil
}

// kernelHome is one kernel's whole home: $JUICE_HOME/kernels/<name>/, holding the database
// (and with it the signing key), config.json, the rail key and its records, the single-server lock
// and the purgeable cache. Everything that binds a kernel to its identity sits in this one
// directory, so it backs up, moves and locks as a unit, and a second kernel on the machine is a
// sibling of the first rather than a second installation.
func kernelHome() string { return filepath.Join(juiceHome(), "kernels", kernelName) }

// legacyKernelHome is the layout before kernels were named, where the root held exactly one. It is
// read only by migrateLegacyHome.
func legacyKernelHome() string { return filepath.Join(juiceHome(), "kernel") }

// migrateLegacyHome moves an unnamed legacy kernel into the named home and is the only writer of that
// path. Both directories live under one root, so the move is a single rename: it either happened or
// it did not, and an interrupted boot leaves no half-moved ledger. It refuses rather than merges
// when a server still holds the old home or when the destination already exists, since either case
// means two kernels are in play and only the operator can say which is wanted. Running it again
// finds nothing to move.
func migrateLegacyHome() error {
	legacy := legacyKernelHome()
	if _, err := os.Stat(filepath.Join(legacy, "juice.db")); err != nil {
		return nil // no legacy kernel here
	}
	dest := kernelHome()
	if _, err := os.Stat(dest); err == nil {
		return kernel.ErrInvalidState.Wrapf(
			"both %s and %s hold a kernel; move or remove one, since only you can say which this installation serves", legacy, dest)
	}
	// The old home's own lock is what a running server holds. Taking it proves nothing is serving
	// that ledger, so the rename cannot pull the database out from under a live process.
	lock, err := os.OpenFile(filepath.Join(legacy, "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return kernel.ErrInvalidState.Wrapf("lock %s: %v", legacy, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return kernel.ErrInvalidState.Wrapf("a server is still running for %s; stop it before this kernel moves to %s", legacy, dest)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := os.MkdirAll(filepath.Join(juiceHome(), "kernels"), 0o700); err != nil {
		return kernel.ErrInvalidState.Wrapf("create %s: %v", filepath.Dir(dest), err)
	}
	if err := os.Rename(legacy, dest); err != nil {
		return kernel.ErrInvalidState.Wrapf("move %s to %s: %v", legacy, dest, err)
	}
	fmt.Fprintf(os.Stderr, "moved kernel home %s to %s\n", legacy, dest)
	return nil
}
