package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSMTPAuthenticatedTLSModes(t *testing.T) {
	for _, mode := range []string{"tls", "starttls"} {
		t.Run(mode, func(t *testing.T) {
			certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
			defer certificateServer.Close()
			certificate := certificateServer.TLS.Certificates[0]
			roots := x509.NewCertPool()
			roots.AddCert(certificateServer.Certificate())
			serverTLS := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			result := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					result <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				secure := mode == "tls"
				if secure {
					conn = tls.Server(conn, serverTLS)
				}
				reader := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 localhost ready\r\n")
				authenticated := false
				delivered := false
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						result <- err
						return
					}
					line = strings.TrimSpace(line)
					switch {
					case strings.HasPrefix(line, "EHLO"):
						if secure {
							fmt.Fprint(conn, "250-localhost\r\n250 AUTH PLAIN\r\n")
						} else {
							fmt.Fprint(conn, "250-localhost\r\n250 STARTTLS\r\n")
						}
					case line == "STARTTLS":
						fmt.Fprint(conn, "220 start tls\r\n")
						secured := tls.Server(conn, serverTLS)
						if err := secured.Handshake(); err != nil {
							result <- err
							return
						}
						conn = secured
						reader = bufio.NewReader(conn)
						secure = true
					case strings.HasPrefix(line, "AUTH PLAIN "):
						value, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
						if !secure || err != nil || string(value) != "\x00user\x00pass" {
							result <- fmt.Errorf("invalid encrypted authentication")
							return
						}
						authenticated = true
						fmt.Fprint(conn, "235 authenticated\r\n")
					case strings.HasPrefix(line, "MAIL FROM:"):
						if !authenticated {
							result <- fmt.Errorf("missing authentication")
							return
						}
						fmt.Fprint(conn, "250 ok\r\n")
					case strings.HasPrefix(line, "RCPT TO:"):
						fmt.Fprint(conn, "250 ok\r\n")
					case line == "DATA":
						fmt.Fprint(conn, "354 send data\r\n")
						data, err := io.ReadAll(textproto.NewReader(reader).DotReader())
						if err != nil {
							result <- err
							return
						}
						if !strings.Contains(string(data), "Message-ID:") {
							result <- fmt.Errorf("missing message headers")
							return
						}
						delivered = true
						fmt.Fprint(conn, "250 accepted\r\n")
					case line == "QUIT":
						fmt.Fprint(conn, "221 bye\r\n")
						if !delivered {
							result <- fmt.Errorf("missing delivery")
						} else {
							result <- nil
						}
						return
					default:
						result <- fmt.Errorf("unexpected command: %s", line)
						return
					}
				}
			}()
			host, port, _ := net.SplitHostPort(listener.Addr().String())
			n, _ := strconv.Atoi(port)
			cfg := testSettings()
			cfg.Host = host
			cfg.Port = n
			cfg.Security = mode
			cfg.Username = "user"
			cfg.Password = "pass"
			if err := sendSMTPWithTLS(context.Background(), cfg, message(), &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSMTPContextCancellationInterruptsRead(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	cfg := testSettings()
	cfg.Host = host
	cfg.Port = n
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- sendSMTP(ctx, cfg, message()) }()
	conn := <-accepted
	defer conn.Close()
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled delivery succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled smtp remained blocked")
	}
}
