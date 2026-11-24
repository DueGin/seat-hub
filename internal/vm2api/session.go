package vm2api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type panelKeys struct {
	owner string
	call  func(context.Context, string, string, url.Values, any, any) error
}

type panelSession struct {
	mu       sync.Mutex
	token    string
	password [32]byte
	expires  time.Time
}

func (client *HTTPClient) ForUser(ctx context.Context, userID, username, password string) (KeyClient, error) {
	if userID == "" || username == "" || password == "" {
		return nil, errors.New("incomplete panel credentials")
	}
	value, _ := client.sessions.LoadOrStore(username, &panelSession{})
	session := value.(*panelSession)
	passwordHash := sha256.Sum256([]byte(password))
	login := func(ctx context.Context, rejected string) (string, error) {
		session.mu.Lock()
		defer session.mu.Unlock()
		if session.token != "" && session.token != rejected && session.password == passwordHash && time.Now().Before(session.expires) {
			return session.token, nil
		}
		if session.token == rejected {
			session.token = ""
		}
		var reply struct {
			Token     string `json:"token"`
			ExpiresIn int    `json:"expires_in"`
		}
		if err := client.callAs(ctx, "", http.MethodPost, "/login", nil, map[string]string{"username": username, "password": password}, &reply); err != nil {
			return "", fmt.Errorf("log in to upstream panel: %w", err)
		}
		if !strings.HasPrefix(reply.Token, "kin-panel-") {
			return "", errors.New("upstream login returned no panel token")
		}
		ttl := time.Duration(reply.ExpiresIn) * time.Second
		if ttl <= 0 {
			ttl = 7 * 24 * time.Hour
		}
		session.token, session.password, session.expires = reply.Token, passwordHash, time.Now().Add(ttl)
		return reply.Token, nil
	}
	if _, err := login(ctx, ""); err != nil {
		return nil, err
	}
	return &panelKeys{owner: userID, call: func(ctx context.Context, method, path string, query url.Values, input, output any) error {
		token, err := login(ctx, "")
		if err != nil {
			return err
		}
		err = client.callAs(ctx, token, method, path, query, input, output)
		if !IsStatus(err, 401) {
			return err
		}
		token, err = login(ctx, token)
		if err != nil {
			return err
		}
		return client.callAs(ctx, token, method, path, query, input, output)
	}}, nil
}
