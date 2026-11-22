// Package email implements the server's outbound SMTP sender
// (notifications: first-run administrator credentials, 2FA recovery
// codes).
//
// Design contract (config.EmailConfig): delivery is BEST-EFFORT. A
// misconfigured or unreachable mail server must never prevent the
// server from starting, block a request, or lose a credential. Every
// send is timeout-bounded; the first-run flow falls back to the
// one-time journal log entry when the send fails, and every other
// caller surfaces the same information in the UI.
//
// Transport: standard library net/smtp only. Port 465 uses implicit
// TLS; other ports start with STARTTLS when the server advertises it
// (mandatory on 587). AUTH PLAIN is used when a username is set.
package email

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/config"
)

// DefaultSendTimeout bounds one send (dial + TLS + SMTP transaction).
const DefaultSendTimeout = 20 * time.Second

// DefaultPort is used when config gives none: 587 (submission + STARTTLS).
const DefaultPort = 587

// Sender delivers plain-text mail to a single fixed recipient (the
// administrator). The server has exactly one notification audience in
// v1; a per-user fan-out would need a recipients table and is
// deliberately out of scope.
type Sender struct {
	log     *slog.Logger
	cfg     config.EmailConfig
	from    string
	to      string
	Timeout time.Duration
}

// New builds a Sender. An invalid/disabled config yields a Sender
// whose Enabled() is false; every method is safe to call on it.
func New(log *slog.Logger, cfg config.EmailConfig, to string) *Sender {
	if log == nil {
		log = slog.Default()
	}
	if to == "" {
		to = "admin@localhost"
	}
	if cfg.From == "" {
		cfg.From = to
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	return &Sender{log: log, cfg: cfg, from: cfg.From, to: to, Timeout: DefaultSendTimeout}
}

// Enabled reports whether a send could possibly succeed.
func (s *Sender) Enabled() bool { return s.cfg.Enabled && s.cfg.Host != "" }

// To returns the fixed recipient.
func (s *Sender) To() string { return s.to }

// Send delivers one plain-text message and returns on success or
// failure. It never blocks longer than s.Timeout.
func (s *Sender) Send(ctx context.Context, subject, body string) error {
	if !s.Enabled() {
		return fmt.Errorf("email: sending disabled (server.email.enabled)")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// net/smtp takes no context, so the timeout is enforced as a
	// deadline on the connection: every read and write in the SMTP
	// transaction is bounded by it.
	deadline, _ := ctx.Deadline()

	hostport := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	if s.cfg.Port == 465 {
		// Implicit TLS: the TCP connection IS the TLS connection.
		d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: tlsCfg}
		conn, err = d.DialContext(ctx, "tcp", hostport)
		if err != nil {
			return fmt.Errorf("email: tls dial: %w", err)
		}
	} else {
		d := &net.Dialer{Timeout: 5 * time.Second}
		conn, err = d.DialContext(ctx, "tcp", hostport)
		if err != nil {
			return fmt.Errorf("email: dial: %w", err)
		}
	}

	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("email: smtp client: %w", err)
	}
	defer c.Close()

	if s.cfg.Port != 465 {
		if !s.cfg.StartTLS {
			s.log.Warn("email: STARTTLS disabled; SMTP (and any credentials) travel in cleartext")
		} else if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("email: starttls: %w", err)
			}
		} else if s.cfg.Port == 587 {
			return fmt.Errorf("email: port 587 server does not advertise STARTTLS (set start_tls: false only for trusted cleartext relays)")
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("email: auth: %w", err)
		}
	}

	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("email: mail from: %w", err)
	}
	if err := c.Rcpt(s.to); err != nil {
		return fmt.Errorf("email: rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("email: data: %w", err)
	}
	if _, err := io.WriteString(w, compose(s.from, s.to, subject, body)); err != nil {
		return fmt.Errorf("email: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: data close: %w", err)
	}
	s.log.Info("email sent", "to", s.to, "subject", subject)
	return nil
}

// SendAsync fires Send on a goroutine; the only trace of the outcome
// is a log line. Use for notifications that ALSO surface in the UI
// (recovery codes are shown once on screen), so a mail outage must
// not affect the operation.
func (s *Sender) SendAsync(subject, body string) {
	if !s.Enabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
		defer cancel()
		if err := s.Send(ctx, subject, body); err != nil {
			s.log.Error("email send failed (best-effort)", "to", s.to, "subject", subject, "error", err)
		}
	}()
}

// compose renders a minimal RFC 5322 text/plain message. Dot-stuffing
// is handled by net/smtp (w.Close()); headers are ASCII-safe via
// RFC 2047 encoding for non-ASCII subjects.
func compose(from, to, subject, body string) string {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeSubject(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123) + "\r\n")
	b.WriteString("Message-ID: <" + hex.EncodeToString(id) + "@openframe>\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

var asciiSubjectRe = regexp.MustCompile(`^[A-Za-z0-9 .!?:,()&'-]+$`)

func encodeSubject(s string) string {
	if asciiSubjectRe.MatchString(s) {
		return s
	}
	return "?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?"
}
