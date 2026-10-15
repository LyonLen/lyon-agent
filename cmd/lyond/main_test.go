package main

import "testing"

// The default -addr and the token requirement are the only things standing
// between an anonymous listener and remote shell execution, so pin both.
func TestLoopbackOnly(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"127.0.0.2:8080", true},
		{"", false},      // all interfaces
		{":8080", false}, // all interfaces
		{"0.0.0.0:8080", false},
		{"192.168.1.10:8080", false},
		{"example.internal:8080", false}, // unresolvable ⇒ assume reachable
		{"8080", false},                  // malformed ⇒ parse failure path
	} {
		if got := loopbackOnly(tc.addr); got != tc.want {
			t.Errorf("loopbackOnly(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestAuthState(t *testing.T) {
	if s := authState(""); s != "off" {
		t.Errorf("authState(\"\") = %q, want off", s)
	}
	if s := authState("tok"); s != "bearer" {
		t.Errorf("authState(tok) = %q, want bearer", s)
	}
}
