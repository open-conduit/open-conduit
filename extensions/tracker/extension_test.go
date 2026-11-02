package tracker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// A managed webhook is due until it points at the target with ok or
// dry_run; a failure waits a minute before it is retried, and warns the
// entry's doctor check.
func TestHooksRetryAndReport(t *testing.T) {
	var h tracker.Hooks
	now := time.Unix(1700000000, 0)
	if !h.Due("ns", "https://x/linear/eng/e", now) {
		t.Fatal("a new hook is not due")
	}
	h.Set("ns", "https://x/linear/eng/e", "", "hook_permission_denied", now)
	if h.Due("ns", "https://x/linear/eng/e", now.Add(30*time.Second)) || !h.Due("ns", "https://x/linear/eng/e", now.Add(time.Minute)) {
		t.Fatal("a failed hook is not retried after a minute")
	}
	check := inproc.CheckResult{Status: inproc.CheckOK, Code: "ok"}
	if item := h.Item(&check, "ns", "linear/eng/e", true, true); item != "POST /linear/eng/e; hook hook_permission_denied" || check.Code != "hook_permission_denied" {
		t.Fatalf("item %q check %+v", item, check)
	}
	h.Set("ns", "https://x/linear/eng/e", "42", "ok", now)
	if h.Due("ns", "https://x/linear/eng/e", now) || h.ID("ns") != "42" || !h.Due("ns", "https://y/linear/eng/e", now) {
		t.Fatal("an ok hook is due again, or a moved URL is not")
	}
	if item := h.Item(&check, "other", "linear/eng/o", false, true); item != "no webhook_secret (not mounted; reconcile only)" {
		t.Fatalf("item without a secret %q", item)
	}
	permission := errors.New("admin only")
	if tracker.HookCode(permission, permission) != "hook_permission_denied" || tracker.HookCode(errors.New("x"), permission) != "hook_update_failed" {
		t.Fatal("hook codes")
	}
}
