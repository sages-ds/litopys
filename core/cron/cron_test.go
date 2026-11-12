package cron

import (
	"testing"
	"time"
)

func TestDue(t *testing.T) {
	at := func(s string) time.Time {
		x, err := time.ParseInLocation("2006-01-02 15:04", s, Kyiv)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	for _, c := range []struct {
		spec, at string
		want     bool
	}{
		{"0 7 * * *", "2026-11-02 07:00", true},
		{"0 7 * * *", "2026-11-02 07:01", false},
		{"*/15 8-18 * * 1-5", "2026-11-02 09:45", true},  // Monday
		{"*/15 8-18 * * 1-5", "2026-11-01 09:45", false}, // Sunday
		{"30 21 * * 0", "2026-11-01 21:30", true},
		{"30 21 * * 7", "2026-11-01 21:30", true},
		{"0 9 1,15 * *", "2026-11-15 09:00", true},
	} {
		sp, err := Parse(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := sp.Due(at(c.at)); got != c.want {
			t.Errorf("%s at %s: %v, want %v", c.spec, c.at, got, c.want)
		}
	}
	for _, bad := range []string{"* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "a * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
