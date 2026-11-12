package main

import (
	"context"
	"strings"
	"testing"
)

func TestListedProfile(t *testing.T) {
	if _, ok := profiles["vault-curated-v1"]; !ok {
		t.Error("vault-curated-v1 is not a profile")
	}
	err := runMigrateFrom(context.Background(), []string{"no-such-profile"})
	if err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Errorf("an unknown profile: %v", err)
	}
}
