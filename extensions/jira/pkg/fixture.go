package jiraext

import (
	"encoding/json"
	"net/http"

	"github.com/open-conduit/open-conduit/extensions/jira/jiratest"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	trackerext "github.com/open-conduit/open-conduit/extensions/tracker/pkg"
)

// fixture is Jira's certification case: a fake Jira site with one open
// issue, and the signed webhook delivery of its creation.
func fixture() (trackerext.Fixture, error) {
	const token = "jira-certify"
	f := jiratest.New("me@example.com", token)
	f.AddIssue("PROJ-1", "The export is broken", "To Do")
	body := []byte(`{"webhookEvent":"jira:issue_created","issue":{"key":"PROJ-1"}}`)
	h := http.Header{}
	h.Set("X-Hub-Signature", tracker.SignHMAC(trackerext.FixtureHookValue, body))
	h.Set("X-Atlassian-Webhook-Identifier", "certify-1")
	conn, _ := json.Marshal(map[string]any{"base_url": f.Server.URL, "email": "me@example.com"})
	return trackerext.Fixture{Conn: conn, Token: token, Header: h, Body: body, Close: f.Close,
		Config: json.RawMessage(`{"namespace":"jira/acme/proj","scope":"` + trackerext.FixtureScope + `","projects":["PROJ"],"transitions":{"done":"Done"},"webhook_secret":"hook"}`)}, nil
}
