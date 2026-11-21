package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
)

// memoryKeyring is a platform keyring in memory. gets counts the values
// read from it, so a test can tell whether anything was resolved.
type memoryKeyring struct {
	mu     sync.Mutex
	values map[string]string
	refuse bool
	gets   int
}

func (k *memoryKeyring) Get(service, name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gets++
	v, ok := k.values[service+"/"+name]
	if !ok {
		return "", errors.New("no such item")
	}
	return v, nil
}
func (k *memoryKeyring) Set(_ context.Context, service, name, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.refuse {
		return errors.New("no keyring")
	}
	k.values[service+"/"+name] = value
	return nil
}
func (k *memoryKeyring) Remove(_ context.Context, service, name string) error {
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

// Resolving secrets on request runs cmd: references and reads the keyring,
// so doctor with check_secrets=true needs admin, as the secrets routes do:
// view and operate are refused before anything is resolved. Without it,
// doctor stays a read for view.
// TestManagementStoredReference covers a reference that doesn't name a store:
// it can be applied before the value exists, is listed against the entry it
// reads, and holds that entry against removal.
func TestManagementStoredReference(t *testing.T) {
	ctx := context.Background()
	h := newManagementHarness(t)
	admin := h.operatorClient()
	h.applyText(strings.Replace(managementConfig, `"other": "env:SECRET_FIXTURE_TOKEN"`, `"other": "stored:model-key"`, 1))
	const pointer = "/scopes/0/secret_refs/other"
	row := func(reference string) client.Secret {
		t.Helper()
		list, err := admin.Secrets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.Reference == reference {
				return s
			}
		}
		t.Fatalf("no row for %s in %+v", reference, list)
		return client.Secret{}
	}
	if s := row("stored:model-key"); s.Resolves || s.Failure != "stored_missing" || !slices.Equal(s.ReferencedBy, []string{pointer}) {
		t.Fatalf("before it is stored: %+v", s)
	}
	if _, err := admin.SetSecret(ctx, "model-key", "in-keyring", ""); err != nil {
		t.Fatal(err)
	}
	if s := row("keyring:model-key"); !s.Resolves || !slices.Equal(s.ReferencedBy, []string{pointer}) {
		t.Fatalf("stored in the keyring: %+v", s)
	}
	var api *client.Error
	if err := admin.DeleteSecret(ctx, "model-key"); !errors.As(err, &api) || api.Code != "secret_in_use" {
		t.Fatalf("removing a name a stored: reference reads: %v", err)
	}
	if _, err := admin.SetSecret(ctx, "model-key", "in-file", "file"); err != nil {
		t.Fatal(err)
	}
	if s := row("file:model-key"); !s.Resolves || !slices.Equal(s.ReferencedBy, []string{pointer}) {
		t.Fatalf("the file's copy is the one read: %+v", s)
	}
	if s := row("keyring:model-key"); len(s.ReferencedBy) != 0 {
		t.Fatalf("the keyring's copy isn't read: %+v", s)
	}
	if _, err := admin.SetSecret(ctx, "model-key", "shadowed", "keyring"); !errors.As(err, &api) || api.Code != "secret_in_file" {
		t.Fatalf("a keyring write the file would shadow: %v", err)
	}
}

// TestManagementGrantSecrets: a secret a grant selects reaches only the runs
// of a Work that took the grant, so a credential that isn't stored yet fails
// only those, and a grant the config later withdraws stops supplying it.
func TestManagementGrantSecrets(t *testing.T) {
	ctx := context.Background()
	const from = `"operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "secret_names": ["api"], "config": {}`
	withGrant := strings.Replace(strings.Replace(managementConfig, `"other": "env:SECRET_FIXTURE_TOKEN"`, `"other": "stored:model-key"`, 1),
		from, from+`, "grants": [{"name": "model", "secret_names": ["other"]}]`, 1)
	if withGrant == managementConfig {
		t.Fatal("the config fixture changed")
	}
	h := newManagementHarnessWith(t, withGrant, nil)
	admin := h.operatorClient()
	secrets := h.configs.Secrets(&config.Resolver{SecretsFile: filepath.Join(h.state, "config", "secrets.json"), Keyring: h.keyring})
	attempt := func(grants ...string) runtime.AttemptState {
		return runtime.AttemptState{ScopeID: "ops", Generation: "1", Binding: domain.Binding{Name: "run"}, Grants: grants}
	}
	if got, err := secrets(ctx, attempt()); err != nil || len(got) != 1 || got["api"] == "" {
		t.Fatalf("a Work without the grant, before the secret is stored: %v %v", got, err)
	}
	if _, err := secrets(ctx, attempt("model")); config.SecretCode(err) != "stored_missing" {
		t.Fatalf("a Work with the grant, before the secret is stored: %v", err)
	}
	if _, err := admin.SetSecret(ctx, "model-key", "sk-canary", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := secrets(ctx, attempt("model")); err != nil || got["other"] != "sk-canary" || got["api"] == "" {
		t.Fatalf("a Work with the grant: %v", err)
	}
	if got, err := secrets(ctx, attempt()); err != nil || got["other"] != "" {
		t.Fatalf("a Work without the grant received its secret (%d names): %v", len(got), err)
	}
	// Withdrawn from the current generation, it stops reaching a run pinned
	// to the generation that offered it.
	h.applyText(strings.Replace(withGrant, `"grants": [{"name": "model", "secret_names": ["other"]}]`, `"grants": [{"name": "model"}]`, 1))
	if got, err := secrets(ctx, attempt("model")); err != nil || got["other"] != "" || got["api"] == "" {
		t.Fatalf("a withdrawn grant still supplied its secret (%d names): %v", len(got), err)
	}
}

func TestManagementDoctorCheckSecretsNeedsAdmin(t *testing.T) {
	ctx := context.Background()
	h := newManagementHarness(t)
	admin := h.operatorClient()
	h.keyring.mu.Lock()
	h.keyring.values["open-conduit/slack-token"] = "tok"
	h.keyring.mu.Unlock()
	h.applyText(strings.Replace(managementConfig, `"other": "env:SECRET_FIXTURE_TOKEN"`, `"other": "keyring:slack-token"`, 1))
	gets := func() int {
		h.keyring.mu.Lock()
		defer h.keyring.mu.Unlock()
		return h.keyring.gets
	}
	secrets := func(d client.Doctor) client.Check {
		for _, c := range d.Checks {
			if c.Name == "secrets" {
				return c
			}
		}
		t.Fatalf("doctor has no secrets check: %+v", d)
		return client.Check{}
	}
	before := gets()
	for _, role := range []string{"view", "operate"} {
		path := filepath.Join(h.state, role+".token")
		h.mustCLI(nil, "auth", "add", "olly-"+role, "--role", role, "--out", path)
		f, err := client.ReadTokenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(h.endpoint, f.Token)
		if err != nil {
			t.Fatal(err)
		}
		var api *client.Error
		if _, err = c.Doctor(ctx, true); !errors.As(err, &api) || api.Status != 403 || api.Code != "forbidden_role" {
			t.Fatalf("%s check_secrets: %v", role, err)
		}
		if n := gets(); n != before {
			t.Fatalf("%s check_secrets resolved %d reference(s) before it was refused", role, n-before)
		}
		d, err := c.Doctor(ctx, false)
		if err != nil {
			t.Fatalf("%s doctor: %v", role, err)
		}
		if s := secrets(d); s.Code != "not_checked" {
			t.Fatalf("%s doctor secrets: %+v", role, s)
		}
	}
	if gets() != before {
		t.Fatal("doctor without check_secrets resolved a reference")
	}
	d, err := admin.Doctor(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if s := secrets(d); s.Code != "resolved" || gets() == before {
		t.Fatalf("admin check_secrets: %+v (keyring reads %d)", s, gets()-before)
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
