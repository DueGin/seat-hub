// Package auth hashes passwords (argon2id), issues portal sessions, and
// provides the HTTP middleware the API and admin routes share.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

func HashPassword(password string) (string, error) {
	if len(password) < 8 || len(password) > 128 {
		return "", errors.New("密码长度须为 8–128")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Limiter is a tiny sliding-window attempt limiter keyed by IP + email. It is
// deliberately in-memory: a single portal instance owns login throttling.
//
// The map is attacker-reachable (every unauthenticated login attempt creates a
// key), so entries are swept lazily and the whole table is bounded: past
// maxLimiterKeys it is dropped wholesale, which costs one window of throttling
// but never unbounded memory.
type Limiter struct {
	mu        sync.Mutex
	buckets   map[string][]time.Time
	limit     int
	window    time.Duration
	maxKeys   int
	lastSweep time.Time
}

// maxLimiterKeys caps the number of distinct keys kept between sweeps.
const maxLimiterKeys = 10000

// maxLimiterKeyLen bounds the caller-controlled part of the key. An email is
// capped at 254 bytes, and the IP prefix adds at most 46, so anything longer
// cannot be a legitimate key.
const maxLimiterKeyLen = 320

func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{
		buckets:   map[string][]time.Time{},
		limit:     limit,
		window:    window,
		maxKeys:   maxLimiterKeys,
		lastSweep: time.Now(),
	}
}

// Allow records an attempt and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	if len(key) > maxLimiterKeyLen {
		key = key[:maxLimiterKeyLen]
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)

	l.sweepLocked(cutoff, now)

	old := l.buckets[key]
	kept := old[:0]
	for _, t := range old {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.buckets[key] = kept
		return false
	}
	l.buckets[key] = append(kept, now)
	return true
}

// sweepLocked drops expired buckets once per window and enforces the cap.
func (l *Limiter) sweepLocked(cutoff, now time.Time) {
	if now.Sub(l.lastSweep) >= l.window {
		for k, times := range l.buckets {
			fresh := 0
			for _, t := range times {
				if t.After(cutoff) {
					fresh++
					break
				}
			}
			if fresh == 0 {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	if len(l.buckets) >= l.maxKeys {
		// Still full after sweeping: start over. Existing attempts lose their
		// history, but only an attacker can drive the table this far.
		l.buckets = map[string][]time.Time{}
		l.lastSweep = now
	}
}

// Len reports the number of tracked keys; used by tests.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ParseInt is a small helper used by handlers for query parameters.
func ParseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
