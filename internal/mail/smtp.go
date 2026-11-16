package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

func buildMessage(cfg Settings, msg Message, now time.Time) ([]byte, error) {
	if err := validateMessage(msg); err != nil {
		return nil, err
	}
	if !Address(cfg.FromAddress) || strings.ContainsAny(cfg.FromName, "\r\n\x00") {
		return nil, ErrInvalid
	}
	var b bytes.Buffer
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate message id: %w", err)
	}
	domain := strings.SplitN(cfg.FromAddress, "@", 2)[1]
	from := (&mail.Address{Name: cfg.FromName, Address: cfg.FromAddress}).String()
	// net/mail uses RFC 2047 Q encoding for non-ASCII display names.
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\n", from, strings.Join(msg.To, ", "), mime.QEncoding.Encode("UTF-8", msg.Subject), now.Format(time.RFC1123Z), hex.EncodeToString(token), domain)
	writePart := func(w io.Writer, body string) error {
		q := quotedprintable.NewWriter(w)
		if _, err := q.Write([]byte(body)); err != nil {
			return err
		}
		return q.Close()
	}
	if msg.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		if err := writePart(&b, msg.Text); err != nil {
			return nil, fmt.Errorf("encode text: %w", err)
		}
	} else {
		writer := multipart.NewWriter(&b)
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", writer.Boundary())
		for _, part := range []struct{ kind, body string }{{"text/plain", msg.Text}, {"text/html", msg.HTML}} {
			p, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {part.kind + "; charset=UTF-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
			if err != nil {
				return nil, fmt.Errorf("create mail part: %w", err)
			}
			if err := writePart(p, part.body); err != nil {
				return nil, fmt.Errorf("encode mail part: %w", err)
			}
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("finish mail: %w", err)
		}
	}
	b.WriteString("\r\n")
	return b.Bytes(), nil
}

func sendSMTP(ctx context.Context, cfg Settings, msg Message) error {
	return sendSMTPWithTLS(ctx, cfg, msg, nil)
}

func sendSMTPWithTLS(ctx context.Context, cfg Settings, msg Message, tlsConfig *tls.Config) error {
	data, err := buildMessage(cfg, msg, time.Now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect smtp: %w", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set smtp deadline: %w", err)
	}
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	if tlsConfig != nil {
		tlsCfg = tlsConfig.Clone()
	}
	if cfg.Security == "tls" {
		secured := tls.Client(conn, tlsCfg)
		if err := secured.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("smtp tls: %w", err)
		}
		conn = secured
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer client.Close()
	if cfg.Security == "starttls" {
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if cfg.Username != "" {
		if err := client.Auth(plainAuth{cfg.Username, cfg.Password}); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(cfg.FromAddress); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	for _, to := range msg.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("smtp recipient: %w", err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp deliver: %w", err)
	}
	// DATA acceptance is authoritative; a failed QUIT must not duplicate delivery.
	_ = client.Quit()
	return nil
}

// Plain authentication is allowed on an explicitly configured plain connection.
// net/smtp.PlainAuth otherwise refuses non-local unencrypted servers.
type plainAuth struct{ username, password string }

func (a plainAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}
func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("unexpected smtp auth challenge")
	}
	return nil, nil
}
