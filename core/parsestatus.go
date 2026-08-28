package core

import "errors"

// StatusWorkLogin is the status every parser reports when a run could not start
// because the tracker session could not be established. It is the name upstream
// uses (AnimeLayerSyncService returns "work_login"), and it replaced three
// spellings that used to mean the same thing: "login failed", a free-text
// "login error: <message>" status, and the string "login failed" returned as a
// parser's result text.
const StatusWorkLogin = "work_login"

// ErrNotAuthorized marks every failure that means "the session is missing,
// rejected, or expired". Handlers key off it to report StatusWorkLogin, so the
// status is derived from the failure itself rather than from whatever each
// parser happened to leave in its result struct.
//
// Wrap it, do not return it bare — the wrapping text is what names the tracker
// and the specific cause:
//
//	fmt.Errorf("korsars: login failed: %w", core.ErrNotAuthorized)
var ErrNotAuthorized = errors.New("not authorized")
