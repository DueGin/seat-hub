package auth

import (
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "correct horse battery" {
		t.Fatal("password stored in clear")
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword(hash, "correct horse batterY") {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword("not-a-hash", "x") {
		t.Fatal("malformed hash accepted")
	}
}

func TestHashPasswordLengthBounds(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("accepted an 5-char password")
	}
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := HashPassword(string(long)); err == nil {
		t.Fatal("accepted a 129-char password")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same-password")
	b, _ := HashPassword("same-password")
	if a == b {
		t.Fatal("two hashes of the same password are identical: salt missing")
	}
}

func TestLimiterWindow(t *testing.T) {
	l := NewLimiter(2, 50*time.Millisecond)
	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("first two attempts rejected")
	}
	if l.Allow("k") {
		t.Fatal("third attempt allowed")
	}
	if !l.Allow("other") {
		t.Fatal("limit is not per key")
	}
	time.Sleep(60 * time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("window did not slide")
	}
}

func TestParseInt(t *testing.T) {
	cases := map[string]int{"": 7, "abc": 7, "12": 12}
	for in, want := range cases {
		if got := ParseInt(in, 7); got != want {
			t.Fatalf("ParseInt(%q) = %d, want %d", in, got, want)
		}
	}
}
