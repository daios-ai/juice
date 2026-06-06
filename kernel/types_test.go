package kernel

import "testing"

func TestActionKindConstants(t *testing.T) {
	if KindHTTP != "http" {
		t.Errorf("KindHTTP: got %q", KindHTTP)
	}
	if KindWasm != "wasm" {
		t.Errorf("KindWasm: got %q", KindWasm)
	}
	if KindNative != "native" {
		t.Errorf("KindNative: got %q", KindNative)
	}
}


func TestProcessStatusConstants(t *testing.T) {
	if ProcessOpen != "open" {
		t.Errorf("ProcessOpen: got %q", ProcessOpen)
	}
	if ProcessClosed != "closed" {
		t.Errorf("ProcessClosed: got %q", ProcessClosed)
	}
}

func TestTxStatusConstants(t *testing.T) {
	if TxSuccess != "success" {
		t.Errorf("TxSuccess: got %q", TxSuccess)
	}
	if TxFailure != "failure" {
		t.Errorf("TxFailure: got %q", TxFailure)
	}
}
