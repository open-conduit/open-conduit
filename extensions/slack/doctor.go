package slack

import (
	"context"
	"strings"
)

// Doctor severities, in increasing order. The host maps them onto its own
// check statuses.
const (
	DoctorOK   = "ok"
	DoctorWarn = "warn"
	DoctorFail = "fail"
)

// AuthReport is one entry's credential check: a severity, a stable code and
// a safe line naming identities and scopes, never a reference or a token.
type AuthReport struct {
	Severity, Code, Item string
}

// CheckAuth is the extension-owned connection test (doctor --check-secrets):
// auth.test with the bot token must belong to the entry's team and bot user,
// and the token's granted scopes are compared with the manifest. Missing
// history or chat:write scopes fail (reconcile or replies cannot work);
// any other missing manifest scope warns, naming what it costs.
func CheckAuth(ctx context.Context, c *Client, s Settings) AuthReport {
	id, err := c.AuthTest(ctx)
	switch {
	case err != nil:
		return AuthReport{DoctorFail, ErrorCode(err), ErrorCode(err)}
	case id.Team != s.Team:
		return AuthReport{DoctorFail, "team_mismatch", "the token belongs to another workspace"}
	}
	r := AuthReport{DoctorOK, "verified", "authenticated as " + id.User}
	raise := func(sev, code string) {
		if rankSeverity(sev) > rankSeverity(r.Severity) {
			r.Severity, r.Code = sev, code
		}
	}
	if s.BotUser != "" && s.BotUser != id.User {
		raise(DoctorWarn, "bot_user_mismatch")
		r.Item += " (bot_user_id is " + s.BotUser + ")"
	}
	if id.Scopes == nil {
		r.Item += "; granted scopes not reported"
		return r
	}
	missing := MissingScopes(id.Scopes)
	if len(missing) == 0 {
		return r
	}
	have := map[string]bool{}
	for _, g := range id.Scopes {
		have[g] = true
	}
	essential := !have["chat:write"] || !have["channels:history"] && !have["groups:history"]
	if essential {
		raise(DoctorFail, "scopes_missing")
	} else {
		raise(DoctorWarn, "scopes_missing")
	}
	r.Item += "; missing scopes " + strings.Join(missing, ", ")
	for _, m := range missing {
		if m == MarkerScope {
			r.Item += " (reply markers are found by the text fallback only)"
		}
	}
	r.Item += ": reinstall the app from the manifest (open-conduit init slack prints its create-app URL)"
	return r
}

func rankSeverity(s string) int {
	switch s {
	case DoctorFail:
		return 2
	case DoctorWarn:
		return 1
	}
	return 0
}
