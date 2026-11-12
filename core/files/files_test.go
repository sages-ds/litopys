package files

import (
	"io"
	"strings"
	"testing"
)

// TestStore: a file is stored once under its sha, read back, and a name that is no sha never opens anything.
func TestStore(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	sha, err := s.Put([]byte("contract scan"))
	if err != nil || len(sha) != 64 {
		t.Fatalf("put: %q %v", sha, err)
	}
	again, _ := s.Put([]byte("contract scan"))
	if again != sha {
		t.Errorf("the same bytes under another sha: %s", again)
	}
	r, err := s.Open(sha)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "contract scan" {
		t.Errorf("read back: %q", b)
	}
	if _, err := s.Open("../../etc/passwd"); err == nil {
		t.Error("a path opened")
	}
	if _, err := s.Put([]byte(strings.Repeat("x", MaxSize+1))); err == nil {
		t.Error("an oversize file was stored")
	}
}
