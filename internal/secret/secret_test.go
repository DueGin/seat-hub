package secret

import (
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := New(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("sk-vm-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "sk-vm-abc123") {
		t.Fatal("ciphertext contains the plaintext")
	}
	plain, err := box.Open(sealed)
	if err != nil || plain != "sk-vm-abc123" {
		t.Fatalf("open = %q, %v", plain, err)
	}
}

func TestSealIsRandomized(t *testing.T) {
	box, _ := New(strings.Repeat("k", 32))
	a, _ := box.Seal("same")
	b, _ := box.Seal("same")
	if a == b {
		t.Fatal("two seals of the same plaintext are identical: nonce reuse")
	}
}

func TestOpenRejectsOtherKey(t *testing.T) {
	box1, _ := New(strings.Repeat("a", 32))
	box2, _ := New(strings.Repeat("b", 32))
	sealed, _ := box1.Seal("secret")
	if _, err := box2.Open(sealed); err == nil {
		t.Fatal("ciphertext opened under a different key")
	}
}

func TestOpenRejectsTampered(t *testing.T) {
	box, _ := New(strings.Repeat("k", 32))
	sealed, _ := box.Seal("secret")
	tampered := []byte(sealed)
	if tampered[len(tampered)-1] == 'A' {
		tampered[len(tampered)-1] = 'B'
	} else {
		tampered[len(tampered)-1] = 'A'
	}
	if _, err := box.Open(string(tampered)); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := New("too-short"); err == nil {
		t.Fatal("accepted a key under 32 bytes")
	}
}

func TestMACIsStableAndKeyed(t *testing.T) {
	box1, _ := New(strings.Repeat("a", 32))
	box2, _ := New(strings.Repeat("b", 32))
	if box1.MAC("sk-pt-x") != box1.MAC("sk-pt-x") {
		t.Fatal("MAC is not deterministic")
	}
	if box1.MAC("sk-pt-x") == box2.MAC("sk-pt-x") {
		t.Fatal("MAC does not depend on the key")
	}
	if box1.MAC("sk-pt-x") == box1.MAC("sk-pt-y") {
		t.Fatal("MAC collides on different inputs")
	}
}

func TestRandomToken(t *testing.T) {
	a, err := RandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := RandomToken(32)
	if a == b {
		t.Fatal("two tokens are identical")
	}
	if len(a) < 40 {
		t.Fatalf("token too short: %d", len(a))
	}
	for _, r := range a {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			t.Fatalf("token has unexpected character %q", r)
		}
	}
}
