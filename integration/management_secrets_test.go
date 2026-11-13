package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/observability"
)

// memoryKeyring is a platform keyring in memory.
type memoryKeyring struct {
	mu     sync.Mutex
	values map[string]string
	refuse bool
}

func (k *memoryKeyring) Get(service, name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.values[service+"/"+name]
	if !ok {
		return "", errors.New("no such item")
	}
	return v, nil
}
func (k *memoryKeyring) Set(service, name, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.refuse {
		return errors.New("no keyring")
	}
	k.values[service+"/"+name] = value
	return nil
}
func (k *memoryKeyring) Remove(service, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.refuse {
		return errors.New("no keyring")
	}
	delete(k.values, service+"/"+name)
	return nil
}

// recordedObserver keeps every record it observes.
type recordedObserver struct {
	mu      sync.Mutex
	records []observability.Record
}

func (o *recordedObserver) Observe(_ context.Context, r observability.Record) error {
	if err := observability.Validate(r); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.records = append(o.records, r)
	return nil
}
func (o *recordedObserver) named(prefix string) []observability.Record {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []observability.Record
	for _, r := range o.records {
		if strings.HasPrefix(r.Name, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// An admin stores, lists and removes secrets over the API: a value goes to
// the keyring (or the file when there is none), is never returned or
// recorded, resolves through the reference the call answers with, and
// can't be removed while the active config reads it. Every call is in the
// operator trail, by name only; other roles are refused.
func TestManagementSecrets(t *testing.T) {
	ctx := context.Background()
	h := newManagementHarness(t)
	admin := h.operatorClient()
	const value = "tok-3f9a-very-secret"

	stored, err := admin.SetSecret(ctx, "slack-token", value, "")
	if err != nil || stored.Store != "keyring" || stored.Reference != "keyring:slack-token" || stored.FellBack {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	if got, _ := h.keyring.Get("open-conduit", "slack-token"); got != value {
		t.Fatalf("keyring holds %q", got)
	}
	// Replacing it is the same call.
	if _, err = admin.SetSecret(ctx, "slack-token", value+"-2", "keyring"); err != nil {
		t.Fatal(err)
	}
	if stored, err = admin.SetSecret(ctx, "hook", "hook-value", "file"); err != nil || stored.Reference != "file:hook" {
		t.Fatalf("file store: %+v %v", stored, err)
	}
	if b, _ := os.ReadFile(filepath.Join(h.state, "config", "secrets.json")); !strings.Contains(string(b), `"hook": "hook-value"`) {
		t.Fatalf("secrets file: %s", b)
	}
	// Without a keyring, the default falls back to the file and says so;
	// asking for the keyring is refused.
	h.keyring.mu.Lock()
	h.keyring.refuse = true
	h.keyring.mu.Unlock()
	if stored, err = admin.SetSecret(ctx, "fallback", "fb", ""); err != nil || stored.Store != "file" || !stored.FellBack {
		t.Fatalf("fallback: %+v %v", stored, err)
	}
	var api *client.Error
	if _, err = admin.SetSecret(ctx, "kr", "v", "keyring"); !errors.As(err, &api) || api.Code != "keyring_refused" {
		t.Fatalf("refused keyring: %v", err)
	}
	h.keyring.mu.Lock()
	h.keyring.refuse = false
	h.keyring.mu.Unlock()
	for _, bad := range []struct{ name, value, code string }{{"Bad Name", "v", "invalid_secret_name"}, {"empty", "", "invalid_secret_value"}} {
		if _, err = admin.SetSecret(ctx, bad.name, bad.value, ""); !errors.As(err, &api) || api.Code != bad.code {
			t.Fatalf("%s: %v", bad.code, err)
		}
	}

	// The config reads one of them.
	h.applyText(strings.Replace(managementConfig, `"other": "env:SECRET_FIXTURE_TOKEN"`, `"other": "keyring:slack-token", "missing": "file:gone"`, 1))
	list, err := admin.Secrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byRef := map[string]client.Secret{}
	for _, s := range list {
		byRef[s.Reference] = s
	}
	if s := byRef["keyring:slack-token"]; !s.Resolves || len(s.ReferencedBy) != 1 || s.ReferencedBy[0] != "/scopes/0/secret_refs/other" {
		t.Fatalf("slack-token: %+v", s)
	}
	if s := byRef["file:hook"]; !s.Resolves || len(s.ReferencedBy) != 0 {
		t.Fatalf("hook: %+v", s)
	}
	if s := byRef["file:gone"]; s.Resolves || s.Failure != "file_missing" {
		t.Fatalf("a referenced name that isn't stored: %+v", s)
	}
	if len(list) != 4 {
		t.Fatalf("listed %+v", list)
	}

	// Removal is refused while the config reads it, then succeeds.
	err = admin.DeleteSecret(ctx, "slack-token")
	if !errors.As(err, &api) || api.Status != 409 || api.Code != "secret_in_use" {
		t.Fatalf("in use: %v", err)
	}
	if got, _ := h.keyring.Get("open-conduit", "slack-token"); got == "" {
		t.Fatal("a refused removal removed the value")
	}
	if err = admin.DeleteSecret(ctx, "hook"); err != nil {
		t.Fatal(err)
	}
	h.applyText(managementConfig)
	if err = admin.DeleteSecret(ctx, "slack-token"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.keyring.Get("open-conduit", "slack-token"); err == nil {
		t.Fatal("keyring still holds the removed name")
	}
	if list, err = admin.Secrets(ctx); err != nil || len(list) != 1 || list[0].Reference != "file:fallback" {
		t.Fatalf("after removal: %+v %v", list, err)
	}

	// Only admin reaches them, listing included.
	path := filepath.Join(h.state, "olly.token")
	h.mustCLI(nil, "auth", "add", "olly", "--role", "operate", "--out", path)
	f, err := client.ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	operator, err := client.New(h.endpoint, f.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = operator.Secrets(ctx); !errors.As(err, &api) || api.Code != "forbidden_role" {
		t.Fatalf("operate listing: %v", err)
	}
	if _, err = operator.SetSecret(ctx, "x", "v", ""); !errors.As(err, &api) || api.Code != "forbidden_role" {
		t.Fatalf("operate storing: %v", err)
	}

	// The trail names each call and its outcome, never a value.
	records := h.observed.named("operator.")
	outcomes := map[string]int{}
	for _, r := range records {
		for _, v := range append([]string{r.Name, r.From, r.To}, valuesOf(r.Attributes)...) {
			if strings.Contains(v, value) || strings.Contains(v, "hook-value") {
				t.Fatalf("a value reached the trail: %+v", r)
			}
		}
		if r.Attributes["operator.actor"] != "operator:local" {
			t.Fatalf("actor: %+v", r)
		}
		outcomes[r.Name+" "+r.Attributes["operator.target"]+" "+r.Attributes["operator.outcome"]]++
	}
	for _, want := range []string{"operator.secret_stored slack-token ok", "operator.secret_stored kr keyring_refused", "operator.secret_removed slack-token secret_in_use", "operator.secret_removed slack-token ok", "operator.secrets_listed  ok"} {
		if outcomes[want] == 0 {
			t.Fatalf("trail lacks %q: %v", want, outcomes)
		}
	}
}

func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// applyText applies config text through the API at the current generation.
func (h *managementHarness) applyText(text string) {
	h.t.Helper()
	ctx := context.Background()
	c := h.operatorClient()
	v, err := c.ValidateConfig(ctx, []byte(text))
	if err != nil {
		h.t.Fatal(err)
	}
	_, gen := h.configs.Current()
	g := strconv.FormatUint(gen, 10)
	if _, err = c.ApplyConfigText(ctx, []byte(text), v.CandidateHash, g, "apply-secrets-"+g); err != nil {
		h.t.Fatal(err)
	}
}
