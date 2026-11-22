package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	migrations "github.com/matthewgall/openframe/migrations"
)

// miniSMTP is the smallest server that can receive one message.
type miniSMTP struct {
	mu       sync.Mutex
	port     int
	lastMsg  string
	lastCode int
}

func startMiniSMTP(t *testing.T, dataReply int) *miniSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &miniSMTP{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				write := func(s string) { c.Write([]byte(s + "\r\n")) }
				write("220 mini")
				r := bufio.NewReader(c)
				inData := false
				var data strings.Builder
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if inData {
						if line == ".\r\n" {
							inData = false
							m.mu.Lock()
							m.lastMsg = data.String()
							m.lastCode = dataReply
							m.mu.Unlock()
							if dataReply >= 200 && dataReply < 300 {
								write("250 OK")
							} else {
								write(fmt.Sprintf("%d rejected", dataReply))
							}
						} else {
							data.WriteString(line)
						}
						continue
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "EHLO"):
						write("250-mini")
						write("250 OK")
					case cmd == "MAIL FROM:", strings.HasPrefix(cmd, "MAIL FROM:"):
						if dataReply >= 200 && dataReply < 300 {
							write("250 OK")
						} else {
							write(fmt.Sprintf("%d rejected", dataReply))
						}
					case strings.HasPrefix(cmd, "RCPT TO:"):
						write("250 OK")
					case cmd == "DATA":
						inData = true
						data.Reset()
						write("354 go")
					case cmd == "QUIT":
						write("221 bye")
						return
					default:
						write("250 OK")
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	m.port = port
	return m
}

func (m *miniSMTP) message() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastMsg
}

func firstRunServer(t *testing.T, logBuf io.Writer, emailCfg config.EmailConfig) *Server {
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, t.TempDir()+"/server.db", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Server.AdminEmail = "admin@example.test"
	cfg.Server.Email = emailCfg
	s, err := New(cfg, slog.New(slog.NewTextHandler(logBuf, nil)), dbh)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestFirstRunEmailsCredential: with a working mail server the
// temporary password arrives by email and NEVER in the logs.
func TestFirstRunEmailsCredential(t *testing.T) {
	m := startMiniSMTP(t, 250)
	var logBuf strings.Builder
	s := firstRunServer(t, &logBuf, config.EmailConfig{
		Enabled: true, Host: "127.0.0.1", Port: m.port, StartTLS: false,
	})
	if err := s.bootstrapFirstAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg := m.message()
	if !strings.Contains(msg, "To: admin@example.test") {
		t.Fatalf("no message to the admin:\n%s", msg)
	}
	if !strings.Contains(msg, "one-time administrator password") {
		t.Fatalf("credential email missing subject line:\n%s", msg)
	}
	// Extract the password from the email and assert it is NOT logged.
	var pass string
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, "Password: ") {
			pass = strings.TrimPrefix(line, "Password: ")
		}
	}
	if pass == "" {
		t.Fatalf("no Password line in email:\n%s", msg)
	}
	if strings.Contains(logBuf.String(), pass) {
		t.Fatal("temporary password leaked into the logs despite successful email delivery")
	}
}

// TestFirstRunEmailFallback: a failing mail server must not strand the
// install — the credential falls back to the journal log entry.
func TestFirstRunEmailFallback(t *testing.T) {
	m := startMiniSMTP(t, 550)
	var logBuf strings.Builder
	s := firstRunServer(t, &logBuf, config.EmailConfig{
		Enabled: true, Host: "127.0.0.1", Port: m.port, StartTLS: false,
	})
	if err := s.bootstrapFirstAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logBuf.String(), "temporary_password") {
		t.Fatalf("fallback log entry missing after mail failure:\n%s", logBuf.String())
	}
}

// TestFirstRunEmailDisabled: with mail off, the behaviour is exactly
// the original documented one (credential in the log).
func TestFirstRunEmailDisabled(t *testing.T) {
	var logBuf strings.Builder
	s := firstRunServer(t, &logBuf, config.EmailConfig{})
	if err := s.bootstrapFirstAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logBuf.String(), "temporary_password") {
		t.Fatalf("log entry missing with mail disabled:\n%s", logBuf.String())
	}
}
