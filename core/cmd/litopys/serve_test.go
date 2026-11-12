package main

import "testing"

func TestListenAddr(t *testing.T) {
	for _, tc := range []struct{ addr, old, want string }{
		{"", "", ":8766"},
		{"", ":9000", ":9000"},
		{":9100", "", ":9100"},
		{":9100", ":9000", ":9100"},
	} {
		t.Setenv("LITOPYS_ADDR", tc.addr)
		t.Setenv("LITOPYS_FACADE_ADDR", tc.old)
		if got := listenAddr(); got != tc.want {
			t.Errorf("LITOPYS_ADDR=%q LITOPYS_FACADE_ADDR=%q: %q, want %q", tc.addr, tc.old, got, tc.want)
		}
	}
}
