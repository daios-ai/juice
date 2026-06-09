package main

import (
	"context"
	"fmt"
	"net/smtp"

	"github.com/daios-ai/juice/kernel"
)

type smtpNotifier struct {
	host     string
	port     int
	user     string
	password string
	from     string
}

func newNotifier(cfg ServerConfig) kernel.Notifier {
	if cfg.SMTPHost == "" {
		return nil
	}
	return &smtpNotifier{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		user:     cfg.SMTPUser,
		password: cfg.SMTPPassword,
		from:     cfg.SMTPFrom,
	}
}

func (s *smtpNotifier) Notify(_ context.Context, to, subject, message string) error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	var auth smtp.Auth
	if s.user != "" {
		auth = smtp.PlainAuth("", s.user, s.password, s.host)
	}
	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s", s.from, to, subject, message)
	return smtp.SendMail(addr, auth, s.from, []string{to}, []byte(body))
}
