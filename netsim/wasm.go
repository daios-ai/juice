// SPDX-License-Identifier: AGPL-3.0-only

package main

import "os"

// A composite is an action that buys other actions inside its own advertised price. It is the case
// most of the money rules are about, and none of them is observable without one: the price bounds
// the whole subtree, both operators are paid at each crossing, and a failure refunds only what the
// subtree did not consume.
//
// These modules are assembled byte by byte rather than compiled, following the fixtures in
// script/wasm_test.go, so the suite needs no TinyGo toolchain to produce a real composite. A module
// imports juice.call and invokes the named actions in order with `{}`, returning the last result.
// Assembling one that calls two actions is what makes a partial refund reachable: the first
// settles, the second fails, and the parent must refund the difference and no more.
func writeComposite(path string, actions ...string) error {
	types := append(append(leb(3),
		0x60, 0x04, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7e), // call: (i32,i32,i32,i32) -> i64
		0x60, 0x01, 0x7f, 0x01, 0x7f, // alloc: (i32) -> i32
		0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e) // run:   (i32,i32) -> i64
	imports := append(append(append(leb(1), vec([]byte("juice"))...), vec([]byte("call"))...), 0x00, 0x00)
	funcs := append(leb(2), 0x01, 0x02)
	mems := append(leb(1), 0x00, 0x01)
	globs := append(append(leb(1), 0x7f, 0x01, 0x41), append(sleb(512), 0x0b)...)
	exports := append(leb(3),
		concat(vec([]byte("memory")), []byte{0x02, 0x00},
			vec([]byte("alloc")), []byte{0x00, 0x01},
			vec([]byte("run")), []byte{0x00, 0x02})...)

	// A bump allocator over one global, which is all the host asks of a module.
	allocBody := []byte{0x01, 0x01, 0x7f, 0x23, 0x00, 0x21, 0x01, 0x23, 0x00,
		0x20, 0x00, 0x6a, 0x24, 0x00, 0x20, 0x01, 0x0b}

	// Action names live at 0 and 64; the shared `{}` argument at 32.
	segs := [][2]any{{32, []byte("{}")}}
	body := []byte{0x00} // no locals
	for i, a := range actions {
		off := 0
		if i > 0 {
			off = 64
		}
		segs = append(segs, [2]any{off, []byte(a)})
		body = append(body, 0x41)
		body = append(body, sleb(off)...)
		body = append(body, 0x41)
		body = append(body, sleb(len(a))...)
		body = append(body, 0x41, 0x20, 0x41, 0x02, 0x10, 0x00) // args ptr, args len, call 0
		if i < len(actions)-1 {
			body = append(body, 0x1a) // drop every result but the last
		}
	}
	body = append(body, 0x0b)

	codes := append(leb(2), concat(vec(allocBody), vec(body))...)
	data := leb(len(segs))
	for _, s := range segs {
		data = append(data, 0x00, 0x41)
		data = append(data, sleb(s[0].(int))...)
		data = append(data, 0x0b)
		data = append(data, vec(s[1].([]byte))...)
	}

	mod := concat([]byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00},
		section(1, types), section(2, imports), section(3, funcs), section(5, mems),
		section(6, globs), section(7, exports), section(10, codes), section(11, data))
	return os.WriteFile(path, mod, 0o644)
}

// leb encodes an unsigned integer the way the WebAssembly format does.
func leb(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

// sleb encodes a signed integer, which is what an i32.const operand is. Encoding one as unsigned
// works until a value has bit six set in its final byte — 64 through 127, and every second such
// range above — at which point it is read as negative. A data segment placed at offset 64 then
// lands at -64 and the module will not instantiate. Section sizes and vector lengths are unsigned
// and keep using leb.
func sleb(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		done := (n == 0 && b&0x40 == 0) || (n == -1 && b&0x40 != 0)
		if !done {
			b |= 0x80
		}
		out = append(out, b)
		if done {
			return out
		}
	}
}

func vec(d []byte) []byte                    { return append(leb(len(d)), d...) }
func section(id byte, payload []byte) []byte { return append([]byte{id}, vec(payload)...) }

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
