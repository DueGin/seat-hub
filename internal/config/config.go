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

	// In-site update (internal/update). The update directory itself comes
	// from update.Dir(), because the launcher needs it before config loads.
	UpdateRepo  string // owner/name of the GitHub repository with releases
	UpdateAPI   string // GitHub API base, overridable for mirrors
	UpdateToken string // optional GitHub token (higher API rate limit)
	// UpdateDrainSeconds bounds how long a restart waits for in-flight
	// requests (long streams included) before closing them.
	UpdateDrainSeconds int
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

		UpdateRepo:         strings.TrimSpace(env("PORTAL_UPDATE_REPO", "DueGin/seat-hub")),
		UpdateAPI:          strings.TrimRight(strings.TrimSpace(env("PORTAL_UPDATE_API", "https://api.github.com")), "/"),
		UpdateToken:        strings.TrimSpace(env("PORTAL_UPDATE_TOKEN", "")),
		UpdateDrainSeconds: envInt("PORTAL_UPDATE_DRAIN_SECONDS", 20),
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
	if strings.Count(c.UpdateRepo, "/") != 1 || strings.HasPrefix(c.UpdateRepo, "/") || strings.HasSuffix(c.UpdateRepo, "/") {
		return c, fmt.Errorf("PORTAL_UPDATE_REPO must be owner/name: %q", c.UpdateRepo)
	}
	if u, err := url.Parse(c.UpdateAPI); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return c, fmt.Errorf("PORTAL_UPDATE_API is not a valid URL: %q", c.UpdateAPI)
	}
	if c.UpdateDrainSeconds < 0 || c.UpdateDrainSeconds > 600 {
		c.UpdateDrainSeconds = 20
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
