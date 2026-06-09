package main

import "testing"

func TestNewNotifier_NilWhenSMTPHostEmpty(t *testing.T) {
	cfg := DefaultServerConfig()
	if newNotifier(cfg) != nil {
		t.Error("expected nil notifier when SMTPHost is empty")
	}
}

func TestNewNotifier_NonNilWhenSMTPHostSet(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.SMTPHost = "mail.example.com"
	cfg.SMTPPort = 587
	cfg.SMTPFrom = "noreply@example.com"
	if newNotifier(cfg) == nil {
		t.Error("expected non-nil notifier when SMTPHost is set")
	}
}
