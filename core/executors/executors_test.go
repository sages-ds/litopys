package executors

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// TestPresets: every vendor has an adapter, a class and its help from domain/executor-keys.md; a training vendor
// starts public; fields fill the URL and refuse what does not belong in one.
func TestPresets(t *testing.T) {
	if len(Presets()) < 8 {
		t.Fatalf("presets: %d", len(Presets()))
	}
	for _, p := range Presets() {
		if p.Adapter == "" || p.DataClass == "" || !strings.Contains(p.Help, "`"+p.Key+"`") {
			t.Errorf("preset %s: %+v", p.Key, p)
		}
		if p.DataPolicy["training"] && p.DataClass != "public" {
			t.Errorf("preset %s trains on data but starts %s", p.Key, p.DataClass)
		}
	}
	cf, _ := Lookup("cloudflare")
	if u, err := cf.URL(map[string]string{"account_id": "abc123"}); err != nil || !strings.Contains(u, "/accounts/abc123/") {
		t.Errorf("cloudflare url: %q %v", u, err)
	}
	if _, err := cf.URL(map[string]string{"account_id": "x/../../evil"}); err == nil {
		t.Error("a field with a slash was accepted")
	}
	if _, err := cf.URL(nil); err == nil {
		t.Error("a missing field was accepted")
	}
}

// TestSealer: a sealed key opens with the same app key and the same executor only; no app key — ErrNoAppKey.
func TestSealer(t *testing.T) {
	if _, err := NewSealer(""); err != ErrNoAppKey {
		t.Fatalf("no app key: %v", err)
	}
	if _, err := NewSealer(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("a short app key was accepted")
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	s, err := NewSealer(base64.StdEncoding.EncodeToString(k))
	if err != nil {
		t.Fatal(err)
	}
	nonce, sealed, err := s.Seal("gsk_test", []byte("executor-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "gsk_test") {
		t.Fatal("the key is readable in the sealed bytes")
	}
	if got, err := s.Open(nonce, sealed, []byte("executor-1")); err != nil || got != "gsk_test" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := s.Open(nonce, sealed, []byte("executor-2")); err == nil {
		t.Fatal("a key opened for another executor")
	}
	if Fingerprint("gsk_test") == Fingerprint("gsk_tesu") || len(Fingerprint("x")) != 8 {
		t.Fatal("fingerprint")
	}
}
