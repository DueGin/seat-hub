// Package config loads portal settings from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Listen        string
	PublicBaseURL string // no trailing slash; empty = derive from request
	DBPath        string
	SecretKey     string
	AdminEmail    string
	AdminPassword string
	Dev           bool
	MaxBodyMB     int

	VM2APIBaseURL   string
	VM2APIMasterKey string
}

func Load() (Config, error) {
	c := Config{
		Listen:          env("PORTAL_LISTEN", ":8790"),
		PublicBaseURL:   strings.TrimRight(env("PORTAL_PUBLIC_BASE_URL", ""), "/"),
		DBPath:          env("PORTAL_DB_PATH", "./data/portal.db"),
		SecretKey:       env("PORTAL_SECRET_KEY", ""),
		AdminEmail:      strings.TrimSpace(env("PORTAL_ADMIN_EMAIL", "")),
		AdminPassword:   env("PORTAL_ADMIN_PASSWORD", ""),
		Dev:             env("PORTAL_DEV", "") == "1",
		MaxBodyMB:       envInt("PORTAL_MAX_BODY_MB", 128),
		VM2APIBaseURL:   strings.TrimRight(env("VM2API_BASE_URL", ""), "/"),
		VM2APIMasterKey: env("VM2API_MASTER_KEY", ""),
	}
	if len(c.SecretKey) < 32 {
		return c, fmt.Errorf("PORTAL_SECRET_KEY must be at least 32 bytes")
	}
	if c.VM2APIBaseURL == "" {
		return c, fmt.Errorf("VM2API_BASE_URL is required")
	}
	if u, err := url.Parse(c.VM2APIBaseURL); err != nil || u.Host == "" {
		return c, fmt.Errorf("VM2API_BASE_URL is not a valid URL: %q", c.VM2APIBaseURL)
	}
	if c.VM2APIMasterKey == "" {
		return c, fmt.Errorf("VM2API_MASTER_KEY is required")
	}
	if c.PublicBaseURL != "" {
		if u, err := url.Parse(c.PublicBaseURL); err != nil || u.Host == "" {
			return c, fmt.Errorf("PORTAL_PUBLIC_BASE_URL is not a valid URL: %q", c.PublicBaseURL)
		}
	}
	if c.MaxBodyMB <= 0 {
		c.MaxBodyMB = 128
	}
	return c, nil
}

// SecureCookies reports whether the public URL is https (or whether we are in
// production without a public URL, in which case TLS is expected in front).
func (c Config) SecureCookies() bool {
	if c.PublicBaseURL != "" {
		return strings.HasPrefix(c.PublicBaseURL, "https://")
	}
	return !c.Dev
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}
