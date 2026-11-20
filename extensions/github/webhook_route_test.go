package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

// An entry's own route answers 421 for a delivery about another
// repository, rather than acknowledging and dropping it, and records it.
func TestEntryWebhookAnswersAMisroute(t *testing.T) {
	c := &clock{t: time.Now()}
	x := New()
	var delivered []string
	x.host = inproc.Host{Now: c.now,
		Secrets: func(context.Context, domain.ID, string, []string) (map[string]string, error) {
			return map[string]string{"hook": "s3cret"}, nil
		},
		DeliverAdded: func(_ context.Context, name string, d extension.Delivery) ([]domain.AcceptedEvent, []bool, error) {
			delivered = append(delivered, name+" "+d.ID)
			return []domain.AcceptedEvent{{}}, []bool{true}, nil
		}}
	s := Settings{Namespace: "github/eng/widgets", Scope: "eng", Repo: "octo/widgets", WebhookSecret: "hook"}
	x.own.entries[s.Namespace] = &entry{name: "github/eng/widgets", scope: "eng", ingress: NewIngress(s, nil, c.now, IngressOptions{})}
	h := x.Webhook("eng/widgets")
	if h == nil || x.Webhook("app") != nil {
		t.Fatal("routes: the entry's should be mounted and no connection's")
	}
	post := func(id, repo string) *httptest.ResponseRecorder {
		body := []byte(`{"action":"opened","repository":{"full_name":"` + repo + `"}}`)
		req := httptest.NewRequest(http.MethodPost, "/github/eng/widgets", strings.NewReader(string(body)))
		req.Header = githubtest.Sign("s3cret", "issues", id, body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := post("d-1", "octo/widgets"); w.Code != http.StatusAccepted || w.Body.String() != "{\"entries\":1,\"duplicate\":false}\n" {
		t.Fatalf("own repository: %d %q", w.Code, w.Body.String())
	}
	if w := post("d-2", "octo/gadgets"); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("another repository: %d", w.Code)
	}
	if len(delivered) != 1 || delivered[0] != "github/eng/widgets d-1" {
		t.Fatalf("delivered %v", delivered)
	}
	if items := x.unmatchedItems(); len(items) != 1 || !strings.Contains(items[0], "POST /github/eng/widgets: 1 deliveries for no configured entry (answered 421), the last issues for octo/gadgets") {
		t.Fatalf("recorded %v", items)
	}
}
