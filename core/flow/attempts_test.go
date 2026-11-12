package flow

import "testing"

// TestMaxAttemptsDefault: a step without a retry rule gets DefaultAttempts, not unlimited (2026-11-01: 45 attempts of
// one failing model step); retry.max still decides when given.
func TestMaxAttemptsDefault(t *testing.T) {
	if got := (Step{}).MaxAttempts(); got != DefaultAttempts {
		t.Errorf("no retry rule: %d, want %d", got, DefaultAttempts)
	}
	zero, five := 0, 5
	s := Step{}
	s.Retry = &struct {
		Max *int `json:"max,omitempty"`
	}{Max: &zero}
	if got := s.MaxAttempts(); got != 1 {
		t.Errorf("retry.max 0: %d, want 1", got)
	}
	s.Retry.Max = &five
	if got := s.MaxAttempts(); got != 6 {
		t.Errorf("retry.max 5: %d, want 6", got)
	}
}
