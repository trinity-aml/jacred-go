package core

import "strings"

// CookieNames reduces a cookie or Set-Cookie string to the names it carries,
// so a failure can be logged without publishing the values.
//
// A cookie value is a credential: replaying it is indistinguishable from having
// logged in. That applies to a *failed* login too — the response usually still
// carries a fresh session, and on a CF-fronted tracker it can carry a
// cf_clearance, which is replayable on its own. Logs live in Data/log for 14
// days, so a value written there outlives the session that produced it.
//
// The name alone is what makes a failure diagnosable ("no bb_session came
// back"), and the name alone is what may be logged.
func CookieNames(cookies ...string) string {
	seen := make(map[string]bool)
	var names []string
	for _, raw := range cookies {
		// Handles both "a=1; b=2" request cookies and a Set-Cookie line, whose
		// attributes (Path, Expires, HttpOnly…) follow the value after ';'.
		for _, part := range strings.Split(raw, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name, _, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			name = strings.TrimSpace(name)
			if name == "" || isCookieAttribute(name) || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// isCookieAttribute filters the Set-Cookie attributes that would otherwise be
// reported as cookie names.
func isCookieAttribute(name string) bool {
	switch strings.ToLower(name) {
	case "path", "domain", "expires", "max-age", "samesite", "secure", "httponly", "partitioned", "priority":
		return true
	}
	return false
}
