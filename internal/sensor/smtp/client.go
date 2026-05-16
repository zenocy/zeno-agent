// Package smtp wraps stdlib net/smtp behind a Client interface so the
// V2.8.0 action surface can `send_reply` / `forward` without taking on
// a new third-party dependency. Only outbound SMTP submission is
// covered — Zeno never operates as an SMTP server.
//
// Production uses RealClient which dials, runs STARTTLS when configured,
// authenticates with PLAIN, and submits the raw RFC 5322 message via
// the standard MAIL FROM / RCPT TO / DATA exchange. Tests inject a
// stub Client through the Executor's struct field.
package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/config"
)

// Client is the small surface the action Executors depend on.
type Client interface {
	Send(ctx context.Context, from string, to []string, raw []byte) error
}

// RealClient wraps stdlib net/smtp.
type RealClient struct {
	Cfg     config.SMTPConfig
	// Now is overridable for tests; only used for the Date check below.
	Now func() time.Time
}

// New returns a configured RealClient. Empty cfg.Host returns nil,
// signaling SMTP is disabled — callers must handle nil to allow the
// "drafts-only" deployment posture.
func New(cfg config.SMTPConfig) *RealClient {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	return &RealClient{Cfg: cfg, Now: time.Now}
}

// Send submits raw to the configured SMTP server with the given
// envelope sender + recipients. ctx is honored for the dial; once the
// SMTP exchange begins net/smtp does not expose a context hook, so a
// late cancel races the server response.
func (r *RealClient) Send(ctx context.Context, from string, to []string, raw []byte) error {
	if from == "" {
		return errors.New("smtp: from is required")
	}
	if len(to) == 0 {
		return errors.New("smtp: at least one recipient required")
	}

	addr := fmt.Sprintf("%s:%d", r.Cfg.Host, r.Cfg.Port)

	dialer := &net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial smtp %s: %w", addr, err)
	}
	defer conn.Close()

	tlsCfg := &tls.Config{ServerName: r.Cfg.Host, MinVersion: tls.VersionTLS12}
	mode := strings.ToLower(strings.TrimSpace(r.Cfg.TLS))

	var c *smtp.Client
	switch mode {
	case "implicit", "tls":
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("tls handshake: %w", err)
		}
		c, err = smtp.NewClient(tlsConn, r.Cfg.Host)
	default:
		c, err = smtp.NewClient(conn, r.Cfg.Host)
	}
	if err != nil {
		return fmt.Errorf("smtp NewClient: %w", err)
	}
	defer func() { _ = c.Quit() }()

	if mode == "" || mode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}

	if r.Cfg.Username != "" {
		auth := smtp.PlainAuth("", r.Cfg.Username, r.Cfg.Password, r.Cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}

	if err := c.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", rcpt, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := wc.Write(raw); err != nil {
		_ = wc.Close()
		return fmt.Errorf("write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("close DATA: %w", err)
	}
	return nil
}

// FromAddress returns the From address the executors should put in the
// message header. Falls back to Username when From is empty.
func FromAddress(cfg config.SMTPConfig) string {
	if cfg.From != "" {
		return cfg.From
	}
	return cfg.Username
}
