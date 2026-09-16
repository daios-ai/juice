// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
)

// The composite the suite publishes must be a module the kernel's own runtime will load. Checking
// that here rather than discovering it mid-run is the difference between a failed assembly and a
// run that silently publishes an action nobody can call.
func TestCompositeIsALoadableModule(t *testing.T) {
	for _, actions := range [][]string{
		{"alice/inner"},
		{"cara/quote", "cara/badout"},
		{"ana@hub/echo"},
	} {
		path := filepath.Join(t.TempDir(), "c.wasm")
		if err := writeComposite(path, actions...); err != nil {
			t.Fatalf("writeComposite(%v): %v", actions, err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Instantiating, not merely compiling. Compilation accepts a module whose data segments
		// fall outside its memory; only instantiation places them, and a composite that compiles
		// but will not instantiate publishes an action nobody can call.
		ctx := context.Background()
		r := wazero.NewRuntime(ctx)
		host := r.NewHostModuleBuilder("juice")
		host.NewFunctionBuilder().
			WithFunc(func(a, b, c, d uint32) uint64 { return 0 }).Export("call")
		if _, err := host.Instantiate(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Instantiate(ctx, b); err != nil {
			t.Fatalf("the module for %v does not instantiate: %v", actions, err)
		}
		r.Close(ctx)
	}
}

// The action names must survive into the module: a composite that compiles but calls the wrong
// action would produce a plausible-looking run that measured nothing.
func TestCompositeCarriesItsActionNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.wasm")
	if err := writeComposite(path, "cara/quote", "cara/badout"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"cara/quote", "cara/badout", "{}"} {
		if !contains(b, want) {
			t.Errorf("the module does not carry %q", want)
		}
	}
}

func contains(hay []byte, needle string) bool {
	n := []byte(needle)
	for i := 0; i+len(n) <= len(hay); i++ {
		if string(hay[i:i+len(n)]) == needle {
			return true
		}
	}
	return false
}

// An i32.const operand is signed. The values in the range 64..127 are the ones an unsigned encoder
// gets wrong, and 64 is the offset the two-action composite actually uses.
func TestSignedEncodingMatchesTheFormat(t *testing.T) {
	for _, c := range []struct {
		in   int
		want []byte
	}{{0, []byte{0}}, {1, []byte{1}}, {32, []byte{32}}, {63, []byte{63}},
		{64, []byte{0xc0, 0x00}}, {127, []byte{0xff, 0x00}}, {512, []byte{0x80, 0x04}}} {
		if got := sleb(c.in); string(got) != string(c.want) {
			t.Errorf("sleb(%d) = % x, want % x", c.in, got, c.want)
		}
	}
	if string(sleb(64)) == string(leb(64)) {
		t.Error("signed and unsigned encodings of 64 must differ; treating them as the same is the " +
			"bug that placed a data segment at -64")
	}
}

func TestLebMatchesTheFormat(t *testing.T) {
	for _, c := range []struct {
		in   int
		want []byte
	}{{0, []byte{0}}, {1, []byte{1}}, {127, []byte{127}}, {128, []byte{0x80, 0x01}},
		{512, []byte{0x80, 0x04}}} {
		got := leb(c.in)
		if string(got) != string(c.want) {
			t.Errorf("leb(%d) = % x, want % x", c.in, got, c.want)
		}
	}
}
