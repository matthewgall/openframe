package email

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/config"
)

// mockSMTP is a just-enough SMTP server for testing the client path:
// greeting, EHLO (no STARTTLS advertised), optional AUTH PLAIN,
// MAIL/RCPT/DATA, QUIT. It records the last message body.
type mockSMTP struct {
	ln       net.Listener
	wantAuth string // base64 of "\0user\0pass"; "" disables the check
	mu       sync.Mutex
	lastMsg  string
	errs     []error
}

func startMockSMTP(t *testing.T, wantAuth string) *mockSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mockSMTP{ln: ln, wantAuth: wantAuth}
	go m.serve()
	t.Cleanup(func() { ln.Close() })
	return m
}

func (m *mockSMTP) serve() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		go m.handle(conn)
	}
}

func (m *mockSMTP) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(s string) { conn.Write([]byte(s + "\r\n")) }
	fail := func(format string, a ...any) {
		m.mu.Lock()
		m.errs = append(m.errs, fmt.Errorf(format, a...))
		m.mu.Unlock()
	}
	write("220 mock ESMTP")
	// Read until the end of the session, answering line by line.
	inData := false
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if inData {
				fail("connection lost inside DATA")
			}
			return
		}
		if inData {
			if line == ".\r\n" {
				inData = false
				m.mu.Lock()
				m.lastMsg = data.String()
				m.mu.Unlock()
				write("250 OK queued")
			} else {
				data.WriteString(line)
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		// Uppercase only the verb — base64 arguments are case-sensitive.
		cmd := trimmed
		if i := strings.IndexByte(trimmed, ' '); i > 0 {
			cmd = strings.ToUpper(trimmed[:i]) + trimmed[i:]
		} else {
			cmd = strings.ToUpper(trimmed)
		}
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if m.wantAuth != "" {
				write("250-mock")
				write("250-AUTH PLAIN")
				write("250 OK")
			} else {
				write("250-mock")
				write("250 OK")
			}
		case strings.HasPrefix(cmd, "AUTH PLAIN "):
			got := strings.TrimPrefix(cmd, "AUTH PLAIN ")
			if m.wantAuth == "" || got != m.wantAuth {
				fail("unexpected AUTH: %q", got)
				write("535 auth failed")
				return
			}
			write("235 authenticated")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			write("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			write("250 OK")
		case cmd == "DATA":
			inData = true
			data.Reset()
			write("354 go ahead")
		case cmd == "QUIT":
			write("221 bye")
			return
		default:
			fail("unexpected command: %q", line)
			write("502 unknown")
		}
	}
}

func (m *mockSMTP) host() string { return strings.SplitN(m.ln.Addr().String(), ":", 2)[0] }
func (m *mockSMTP) port() int {
	_, p, _ := net.SplitHostPort(m.ln.Addr().String())
	var n int
	fmt.Sscanf(p, "%d", &n)
	return n
}

func (m *mockSMTP) message() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastMsg
}

func (m *mockSMTP) problems() []error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]error(nil), m.errs...)
}

func testSender(t *testing.T, cfg config.EmailConfig) *Sender {
	t.Helper()
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, "admin@example.test")
	s.Timeout = 3 * time.Second
	return s
}

func TestSendPlain(t *testing.T) {
	m := startMockSMTP(t, "")
	s := testSender(t, config.EmailConfig{
		Enabled: true, Host: m.host(), Port: m.port(),
		StartTLS: false, // mock does not advertise STARTTLS
		From:     "OpenFrame <openframe@example.test>",
	})
	err := s.Send(context.Background(), "OpenFrame test", "hello\nbody line")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, p := range m.problems() {
		t.Error("mock server saw:", p)
	}
	msg := m.message()
	for _, want := range []string{
		"From: OpenFrame <openframe@example.test>",
		"To: admin@example.test",
		"Subject: OpenFrame test",
		"Content-Type: text/plain; charset=utf-8",
		"hello", "body line",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestSendWithAuth(t *testing.T) {
	want := base64.StdEncoding.EncodeToString([]byte("\x00smtp-user\x00smtp-pass"))
	m := startMockSMTP(t, want)
	s := testSender(t, config.EmailConfig{
		Enabled: true, Host: m.host(), Port: m.port(),
		StartTLS: false, Username: "smtp-user", Password: "smtp-pass",
	})
	if err := s.Send(context.Background(), "a", "b"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, p := range m.problems() {
		t.Error("mock server saw:", p)
	}
}

func TestSendDisabled(t *testing.T) {
	s := testSender(t, config.EmailConfig{})
	if s.Enabled() {
		t.Fatal("disabled config must not be enabled")
	}
	if err := s.Send(context.Background(), "a", "b"); err == nil {
		t.Fatal("Send on disabled sender must fail")
	}
	s.SendAsync("a", "b") // must not panic
}

func TestSendRefused(t *testing.T) {
	// Listen then close: the next dial is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	s := testSender(t, config.EmailConfig{
		Enabled: true, Host: "127.0.0.1", Port: port, StartTLS: false,
	})
	if err := s.Send(context.Background(), "a", "b"); err == nil {
		t.Fatal("Send to a refused port must fail")
	}
}

func TestSendTimeout(t *testing.T) {
	// A server that accepts but never speaks: Send must return on the
	// timeout, not hang.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			<-make(chan struct{}) // hold forever, never answer
			_ = c
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	s := testSender(t, config.EmailConfig{
		Enabled: true, Host: "127.0.0.1", Port: port, StartTLS: false,
	})
	s.Timeout = 500 * time.Millisecond
	start := time.Now()
	if err := s.Send(context.Background(), "a", "b"); err == nil {
		t.Fatal("Send to a silent server must fail")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Send took %v, expected bounded by the 500ms timeout", time.Since(start))
	}
}

func TestEncodeSubject(t *testing.T) {
	if got := encodeSubject("OpenFrame: 2FA recovery codes"); got != "OpenFrame: 2FA recovery codes" {
		t.Errorf("ascii subject must be unchanged: %q", got)
	}
	enc := encodeSubject("Wörte")
	if !strings.HasPrefix(enc, "?UTF-8?B?") || !strings.HasSuffix(enc, "?") {
		t.Errorf("non-ascii subject must be RFC 2047 encoded: %q", enc)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(enc, "?UTF-8?B?"), "?"))
	if err != nil || string(raw) != "Wörte" {
		t.Errorf("round trip: %q err=%v", raw, err)
	}
}
