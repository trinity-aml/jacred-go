package main

import "testing"

func TestListenAddress(t *testing.T) {
	for host, expected := range map[string]string{"any": ":9118", "": ":9118", "127.0.0.1": "127.0.0.1:9118", "::1": "[::1]:9118"} {
		if got := listenAddress(host, 9118); got != expected {
			t.Errorf("listenAddress(%q) = %q, want %q", host, got, expected)
		}
	}
}
