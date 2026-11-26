package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

type SMTPConfig struct {
	Address  string
	From     string
	Username string
	Password string
}

type smtpSender struct {
	config SMTPConfig
	host   string
}

func NewSMTPSender(config SMTPConfig) (VerificationSender, error) {
	if config == (SMTPConfig{}) {
		return nil, nil
	}
	host, port, err := net.SplitHostPort(config.Address)
	if err != nil || host == "" || port == "" {
		return nil, errors.New("MAIL_SMTP_ADDR must contain an SMTP host and port")
	}
	config.From, err = verificationEmail(config.From)
	if err != nil {
		return nil, errors.New("MAIL_SMTP_FROM must be a valid email address")
	}
	if (config.Username == "") != (config.Password == "") {
		return nil, errors.New("MAIL_SMTP_USERNAME and MAIL_SMTP_PASSWORD must be set together")
	}
	return smtpSender{config: config, host: host}, nil
}

func (s smtpSender) SendVerificationCode(ctx context.Context, address, code string, validity time.Duration) error {
	return s.SendPlainText(ctx, address, "LiveMedia verification code",
		fmt.Sprintf("Your verification code is %s. It expires in %d seconds.", code, int(validity.Seconds())))
}

func (s smtpSender) SendPlainText(ctx context.Context, address, subject, body string) error {
	address, err := verificationEmail(address)
	if err != nil || subject == "" || strings.ContainsAny(subject, "\r\n") || len(body) > 65536 {
		return errors.New("invalid plain text mail")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.config.Address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if s.config.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.config.Username, s.config.Password, s.host)); err != nil {
			return err
		}
	}
	if err := client.Mail(s.config.From); err != nil {
		return err
	}
	if err := client.Rcpt(address); err != nil {
		return err
	}
	message, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(message, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n",
		s.config.From, address, subject, body); err != nil {
		return err
	}
	// DATA acceptance is the delivery handoff. Closure of the transport then
	// has no effect on whether the SMTP server queued the message.
	return message.Close()
}
