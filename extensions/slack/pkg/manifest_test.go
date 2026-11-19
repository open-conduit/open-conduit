package slackext

import (
	"slices"
	"testing"
)

// The thread's buttons list and answer the actions on a conversation's
// Work and its child Work, and the plan shows the child's requests
// (inproc.Host.Actions, Act and Requests). Over the protocol those are
// action.list, action.submit and request.list, which the host refuses
// unless the manifest has the action.submit permission (issue #759).
func TestManifestEnablesActingForPeople(t *testing.T) {
	enabled := Manifest().Enabled()
	for _, m := range []string{"action.list", "action.submit", "request.list"} {
		if !slices.Contains(enabled, m) {
			t.Errorf("%s is not enabled by the manifest", m)
		}
	}
}
