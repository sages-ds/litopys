// Package files keeps the bytes of attachments in the app's data directory, named by their
// SHA-256 (<dir>/<sha[0:2]>/<sha>): the same file is stored once, a name cannot escape the directory, and the journal
// holds only what describes it.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// MaxSize: the largest attachment, 10 MB.
const MaxSize = 10 << 20

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Store is a data directory of attachments.
type Store struct{ Dir string }

// FromEnv: LITOPYS_FILES_DIR; empty — no store (attachments are off).
func FromEnv() *Store {
	if d := os.Getenv("LITOPYS_FILES_DIR"); d != "" {
		return &Store{Dir: d}
	}
	return nil
}

func (s *Store) path(sha string) (string, error) {
	if !shaRe.MatchString(sha) {
		return "", errors.New("not a sha256")
	}
	return filepath.Join(s.Dir, sha[:2], sha), nil
}

// Put stores b and returns its SHA-256; a file already there is kept.
func (s *Store) Put(b []byte) (string, error) {
	if len(b) > MaxSize {
		return "", fmt.Errorf("file of %d bytes: at most %d", len(b), MaxSize)
	}
	h := sha256.Sum256(b)
	sha := hex.EncodeToString(h[:])
	p, _ := s.path(sha)
	if _, err := os.Stat(p); err == nil {
		return sha, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return sha, os.Rename(tmp.Name(), p)
}

// Open opens a stored file by its SHA-256.
func (s *Store) Open(sha string) (io.ReadCloser, error) {
	p, err := s.path(sha)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}
