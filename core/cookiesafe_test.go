package core

import "testing"

func TestCookieNames(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"request cookie", []string{"uid=12345; pass=deadbeefcafe"}, "uid pass"},
		{"set-cookie attributes dropped", []string{"bb_session=abc; Path=/; HttpOnly; Secure; Max-Age=3600"}, "bb_session"},
		{"duplicates collapsed", []string{"a=1; a=2", "a=3; b=4"}, "a b"},
		{"empty", []string{""}, "none"},
		{"valueless token ignored", []string{"HttpOnly"}, "none"},
	}
	for _, tc := range tests {
		if got := CookieNames(tc.in...); got != tc.want {
			t.Errorf("%s: CookieNames(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The whole point of the helper: no value may survive it.
func TestCookieNamesLeaksNoValues(t *testing.T) {
	secret := "cf_clearance=SECRET_VALUE_XYZ; bb_session=ANOTHER_SECRET; Path=/"
	got := CookieNames(secret)
	for _, v := range []string{"SECRET_VALUE_XYZ", "ANOTHER_SECRET"} {
		if containsStr(got, v) {
			t.Errorf("value %q leaked into %q", v, got)
		}
	}
	if got != "cf_clearance bb_session" {
		t.Errorf("got %q", got)
	}
}

func containsStr(h, n string) bool {
	return len(n) > 0 && len(h) >= len(n) && (func() bool {
		for i := 0; i+len(n) <= len(h); i++ {
			if h[i:i+len(n)] == n {
				return true
			}
		}
		return false
	})()
}
