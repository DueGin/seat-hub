package mail

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
)

func fixture(t *testing.T) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := (&store.DB{DB: db}).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New(strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	return New(db, box, nil)
}
func testSettings() Settings {
	return Settings{Enabled: true, Host: "localhost", Port: 25, Security: "none", FromAddress: "sender@example.com", FromName: "工作台"}
}
func message() Message {
	return Message{To: []string{"user@example.com"}, Subject: "开售通知", Text: "预约商品已开售。\n欢迎查看。"}
}

func TestSettingsSealedMaskAndRecipients(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	cfg := testSettings()
	cfg.Username = "sender"
	cfg.Password = "super-secret-password"
	saved, err := s.SaveSettings(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Password != "***" {
		t.Fatal("password not masked")
	}
	var raw string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='mail'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, cfg.Password) {
		t.Fatal("plaintext stored")
	}
	loaded, err := s.load(ctx, s.db)
	if err != nil || loaded.Password != cfg.Password {
		t.Fatalf("load=%+v err=%v", loaded, err)
	}
	saved.Host = "smtp.example.com"
	if _, err := s.SaveSettings(ctx, saved); err != nil {
		t.Fatal(err)
	}
	loaded, err = s.load(ctx, s.db)
	if err != nil || loaded.Password != cfg.Password {
		t.Fatal("masked password did not survive")
	}
	if _, err := s.db.Exec(`INSERT INTO users(email,password_hash,role,status,created_at,updated_at) VALUES ('one@example.com','','admin','active','',''),('two@example.com','','admin','disabled','',''),('three@example.com','','user','active','','')`); err != nil {
		t.Fatal(err)
	}
	recipients, err := s.AdminRecipients(ctx)
	if err != nil || len(recipients) != 1 || recipients[0] != "one@example.com" {
		t.Fatalf("recipients=%v err=%v", recipients, err)
	}
	saved.AdminEmails = []string{" Other@Example.com "}
	if _, err := s.SaveSettings(ctx, saved); err != nil {
		t.Fatal(err)
	}
	recipients, err = s.AdminRecipients(ctx)
	if err != nil || len(recipients) != 1 || recipients[0] != "other@example.com" {
		t.Fatalf("recipients=%v err=%v", recipients, err)
	}
}
func TestDisabledDropAndValidation(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if err := s.Enqueue(ctx, Message{}); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM mail_outbox`).Scan(&count)
	if count != 0 {
		t.Fatal("disabled email queued")
	}
	for _, update := range []func(*Settings){func(c *Settings) { c.Host = "smtp.example.com\r\nX: bad" }, func(c *Settings) { c.FromAddress = "Name <a@example.com>" }, func(c *Settings) { c.Security = "bad" }, func(c *Settings) { c.Port = 65536 }, func(c *Settings) { c.FromName = "hello\r\nBcc: victim@example.com" }} {
		cfg := testSettings()
		update(&cfg)
		if _, err := s.SaveSettings(ctx, cfg); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted invalid settings: %v", err)
		}
	}
	cfg := testSettings()
	s.SaveSettings(ctx, cfg)
	invalid := message()
	invalid.Subject = "hello\r\nBcc: b@example.com"
	if err := s.Enqueue(ctx, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("header injection accepted: %v", err)
	}
}
func TestBuildMessage(t *testing.T) {
	for _, html := range []string{"", "<p>预约商品已开售。</p>"} {
		t.Run(fmt.Sprint(html != ""), func(t *testing.T) {
			msg := message()
			msg.HTML = html
			now := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
			raw, err := buildMessage(testSettings(), msg, now)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := netmail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
			if err != nil || subject != msg.Subject {
				t.Fatalf("subject=%q err=%v", subject, err)
			}
			from, err := netmail.ParseAddress(parsed.Header.Get("From"))
			if err != nil || from.Name != "工作台" {
				t.Fatalf("from=%v err=%v", from, err)
			}
			if !strings.Contains(parsed.Header.Get("Subject"), "?q?") || !strings.Contains(parsed.Header.Get("From"), "?q?") {
				t.Fatal("missing Q encoding")
			}
			if _, err := netmail.ParseDate(parsed.Header.Get("Date")); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(parsed.Header.Get("Message-ID"), "@example.com>") {
				t.Fatal("invalid message id")
			}
			media, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			if html == "" {
				if media != "text/plain" {
					t.Fatal(media)
				}
				body, err := io.ReadAll(quotedprintable.NewReader(parsed.Body))
				if err != nil || strings.ReplaceAll(strings.TrimSpace(string(body)), "\r\n", "\n") != msg.Text {
					t.Fatalf("body=%q err=%v", body, err)
				}
			} else {
				if media != "multipart/alternative" {
					t.Fatal(media)
				}
				reader := multipart.NewReader(parsed.Body, params["boundary"])
				for _, want := range []string{msg.Text, msg.HTML} {
					part, err := reader.NextPart()
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(part)
					if err != nil || strings.ReplaceAll(string(body), "\r\n", "\n") != want {
						t.Fatalf("part=%q err=%v", body, err)
					}
				}
				if _, err := reader.NextPart(); err != io.EOF {
					t.Fatal(err)
				}
			}
		})
	}
}
func outbox(t *testing.T, s *Service) (string, int, string, string) {
	t.Helper()
	var status, runAfter, lastError string
	var attempts int
	if err := s.db.QueryRow(`SELECT status,attempts,run_after,last_error FROM mail_outbox ORDER BY id LIMIT 1`).Scan(&status, &attempts, &runAfter, &lastError); err != nil {
		t.Fatal(err)
	}
	return status, attempts, runAfter, lastError
}
func TestOutboxBackoffMaxAttemptsAndPrune(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	cfg := testSettings()
	cfg.Password = "private-pass"
	s.SaveSettings(ctx, cfg)
	calls := 0
	s.send = func(context.Context, Settings, Message) error { calls++; return errors.New("rejected private-pass") }
	if err := s.Enqueue(ctx, message()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	for attempt := 1; attempt <= 6; attempt++ {
		if err := s.process(ctx, now); err != nil {
			t.Fatal(err)
		}
		status, got, runAfter, lastError := outbox(t, s)
		want := "pending"
		if attempt == 6 {
			want = "failed"
		}
		if status != want || got != attempt || strings.Contains(lastError, "private-pass") {
			t.Fatalf("status=%s attempts=%d error=%q", status, got, lastError)
		}
		next, err := time.Parse(time.RFC3339, runAfter)
		if err != nil {
			t.Fatal(err)
		}
		expected := now.Add(30 * time.Second * time.Duration(1<<(attempt-1))).Truncate(time.Second)
		if !next.Equal(expected) {
			t.Fatalf("next=%v want=%v", next, expected)
		}
		if err := s.process(ctx, now); err != nil {
			t.Fatal(err)
		}
		if calls != attempt {
			t.Fatal("retried before due")
		}
		now = next.Add(time.Second)
	}
	if calls != 6 {
		t.Fatal(calls)
	}
	if err := s.process(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls != 6 {
		t.Fatal("failed email retried")
	}
	_, err := s.db.Exec(`INSERT INTO mail_outbox(message,status,run_after,created_at,sent_at) VALUES ('{}','sent',?,?,?),('{}','sent',?,?,?)`, now.Format(time.RFC3339), now.Format(time.RFC3339), now.AddDate(0, 0, -31).Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.process(ctx, now); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM mail_outbox WHERE status='sent'`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
}
func TestOutboxSuccessRecoveryAndCancellation(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	s.SaveSettings(ctx, testSettings())
	s.Enqueue(ctx, message())
	var calls atomic.Int32
	s.send = func(context.Context, Settings, Message) error { calls.Add(1); return nil }
	// A process that died after claiming must be retried after its lease expires.
	s.db.Exec(`UPDATE mail_outbox SET status='sending',locked_until=?,attempts=1`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339))
	if err := s.process(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	status, n, _, _ := outbox(t, s)
	if status != "sent" || n != 2 || calls.Load() != 1 {
		t.Fatalf("%s %d %d", status, n, calls.Load())
	}
	if err := s.process(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("sent email resent")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func fakeSMTP(t *testing.T, reject bool) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	messages := make(chan string, 1)
	go func() {
		defer close(messages)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 localhost ready\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(conn, "250-localhost\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(line, "AUTH PLAIN "):
				raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
				if err != nil || string(raw) != "\x00user\x00pass" {
					fmt.Fprint(conn, "535 bad credentials\r\n")
				} else {
					fmt.Fprint(conn, "235 authenticated\r\n")
				}
			case strings.HasPrefix(line, "MAIL FROM:"):
				fmt.Fprint(conn, "250 sender ok\r\n")
			case strings.HasPrefix(line, "RCPT TO:"):
				if reject {
					fmt.Fprint(conn, "550 mailbox unavailable\r\n")
				} else {
					fmt.Fprint(conn, "250 recipient ok\r\n")
				}
			case line == "DATA":
				fmt.Fprint(conn, "354 send message\r\n")
				raw, err := io.ReadAll(textproto.NewReader(reader).DotReader())
				if err != nil {
					return
				}
				messages <- string(raw)
				fmt.Fprint(conn, "250 queued\r\n")
			case line == "QUIT":
				fmt.Fprint(conn, "221 bye\r\n")
				return
			default:
				fmt.Fprint(conn, "500 unknown command\r\n")
			}
		}
	}()
	return listener.Addr().String(), messages
}
func TestSMTPDialogue(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			address, received := fakeSMTP(t, reject)
			host, port, _ := net.SplitHostPort(address)
			n, _ := strconv.Atoi(port)
			s := fixture(t)
			cfg := testSettings()
			cfg.Host = host
			cfg.Port = n
			cfg.Username = "user"
			cfg.Password = "pass"
			if _, err := s.SaveSettings(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			err := s.SendTest(context.Background(), "receiver@example.com")
			if reject {
				if err == nil || (!strings.Contains(err.Error(), "550") || !strings.Contains(err.Error(), "mailbox unavailable")) {
					t.Fatalf("missing exact smtp error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case raw := <-received:
				if !strings.Contains(raw, "receiver@example.com") || !strings.Contains(raw, "Message-ID:") {
					t.Fatal(raw)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing smtp data")
			}
		})
	}
}
func TestSTARTTLSRequiredAndImplicitTLSRejectedPlainServer(t *testing.T) {
	for _, security := range []string{"starttls", "tls"} {
		t.Run(security, func(t *testing.T) {
			address, _ := fakeSMTP(t, false)
			host, port, _ := net.SplitHostPort(address)
			n, _ := strconv.Atoi(port)
			cfg := testSettings()
			cfg.Host = host
			cfg.Port = n
			cfg.Security = security
			if err := sendSMTP(context.Background(), cfg, message()); err == nil {
				t.Fatal("accepted server without required tls")
			}
		})
	}
}

func TestSMTPErrorRedactsPasswordAndAuthPayload(t *testing.T) {
	cfg := testSettings()
	cfg.Username = "user"
	cfg.Password = "secret-value"
	payload := base64.StdEncoding.EncodeToString([]byte("\x00user\x00secret-value"))
	err := redactError(fmt.Errorf("535 rejected %s and %s", cfg.Password, payload), cfg)
	if strings.Contains(err.Error(), cfg.Password) || strings.Contains(err.Error(), payload) || !strings.Contains(err.Error(), "535") {
		t.Fatal(err)
	}
}
