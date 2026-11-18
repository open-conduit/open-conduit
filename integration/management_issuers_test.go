package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/management"
)

// fakeIDP is a credential verifier whose issuer config is its people: each
// token names a subject, groups and a lifetime. It counts its calls.
type fakeIDP struct {
	mu    sync.Mutex
	now   func() time.Time
	calls int
	fail  bool
}

type fakeIDPConfig struct {
	People map[string]struct {
		Subject string   `json:"subject"`
		Groups  []string `json:"groups"`
	} `json:"people"`
}

func (f *fakeIDP) Verify(_ context.Context, c extension.CredentialCheck) (extension.VerifiedCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return extension.VerifiedCredential{}, errors.New("keys unavailable")
	}
	var cfg fakeIDPConfig
	if err := json.Unmarshal(c.Config.Data, &cfg); err != nil {
		return extension.VerifiedCredential{}, err
	}
	p, ok := cfg.People[c.Token]
	if !ok {
		return extension.VerifiedCredential{}, extension.ErrUnauthorized
	}
	return extension.VerifiedCredential{Subject: p.Subject, Groups: p.Groups, ExpiresAt: f.now().Add(10 * time.Minute)}, nil
}

func (f *fakeIDP) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeIDP) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

const idpPeople = `{"people": {
	"tok-alice": {"subject": "alice@example.com", "groups": ["platform", "everyone"]},
	"tok-bob": {"subject": "bob@example.com", "groups": ["everyone"]},
	"tok-carol": {"subject": "carol@example.com", "groups": ["sales"]}
}}`

// TestManagementTrustedIssuers: an admin trusts an issuer; its credentials
// authenticate as operator:<issuer>:<subject> with the role the issuer's
// group table gives their groups, verified once and then cached until they
// expire or the issuer changes; anything else is 401, and only an admin
// changes what an issuer may do.
func TestManagementTrustedIssuers(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	idp := &fakeIDP{now: h.clock.Now}
	h.verifiers = map[string]management.Verifier{"fake": {Verifier: idp, Validate: func(raw json.RawMessage) error {
		var c fakeIDPConfig
		if json.Unmarshal(raw, &c) != nil || len(c.People) == 0 {
			return errors.New("no people")
		}
		return nil
	}}}
	owner := h.operatorClient()
	as := func(token string) *client.Client {
		c, err := client.New(h.endpoint, token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	alice, bob, carol := as("tok-alice"), as("tok-bob"), as("tok-carol")

	// No issuer: a credential the host didn't issue is unknown.
	if _, err := alice.Works(ctx, nil); !isStatus(err, 401) {
		t.Fatalf("an unknown credential without issuers: %v", err)
	}
	if idp.count() != 0 {
		t.Fatal("asked a verifier with no issuer trusted")
	}

	// Only an admin trusts an issuer, of a type a package provides, with
	// a config the package accepts and roles for groups.
	h.mustCLI(nil, "auth", "add", "ops", "--role", "operate", "--out", h.state+"/ops.token")
	opsFile, err := client.ReadTokenFile(h.state + "/ops.token")
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]string{"platform": "operate", "everyone": "view"}
	if _, err = as(opsFile.Token).SetIssuer(ctx, "corp", "fake", json.RawMessage(idpPeople), groups); !isStatus(err, 403) {
		t.Fatalf("an operate operator trusted an issuer: %v", err)
	}
	if _, err = owner.SetIssuer(ctx, "corp", "saml", json.RawMessage(idpPeople), groups); !isStatus(err, 422) {
		t.Fatalf("an unknown verifier type: %v", err)
	}
	if _, err = owner.SetIssuer(ctx, "corp", "fake", json.RawMessage(`{}`), groups); !isStatus(err, 422) {
		t.Fatalf("a config the verifier refuses: %v", err)
	}
	if _, err = owner.SetIssuer(ctx, "Corp", "fake", json.RawMessage(idpPeople), groups); !isStatus(err, 400) {
		t.Fatalf("an invalid issuer ID: %v", err)
	}
	if _, err = owner.SetIssuer(ctx, "corp", "fake", json.RawMessage(idpPeople), map[string]string{"platform": "root"}); !isStatus(err, 400) {
		t.Fatalf("an unknown role: %v", err)
	}
	var set client.Issuer
	h.mustCLI(&set, "auth", "issuer", "set", "corp", "--type", "fake", "--config", idpPeople, "--groups", `{"platform":"operate","everyone":"view"}`)
	if set.ID != "corp" || set.Groups["platform"] != "operate" {
		t.Fatalf("issuer set: %+v", set)
	}

	// alice is operate: she sends an Event, recorded as hers, under a key
	// that is hers alone.
	sent, err := alice.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: "alice-1"}, "shared-key")
	if err != nil {
		t.Fatalf("alice sends an Event: %v", err)
	}
	if sent.Actor != "operator:corp:alice@example.com" {
		t.Fatalf("actor %q", sent.Actor)
	}
	if _, err = owner.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: "owner-1"}, "shared-key"); err != nil {
		t.Fatalf("the owner's request under alice's key: %v", err)
	}
	// She can't do what only an admin can, nor rotate a credential the
	// host didn't issue.
	if _, err = alice.SetIssuer(ctx, "corp", "fake", json.RawMessage(idpPeople), map[string]string{"platform": "admin"}); !isStatus(err, 403) {
		t.Fatalf("alice changed her own group table: %v", err)
	}
	if _, err = alice.Rotate(ctx, ""); !isStatus(err, 409) {
		t.Fatalf("rotated an issuer's credential: %v", err)
	}
	// bob is view: he reads but sends nothing. carol's groups map to no
	// role: she is authenticated and may nothing.
	if _, err = bob.Works(ctx, nil); err != nil {
		t.Fatalf("bob reads: %v", err)
	}
	if _, err = bob.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: "bob-1"}, "bob-1"); !isStatus(err, 403) {
		t.Fatalf("a view operator sent an Event: %v", err)
	}
	if _, err = carol.Works(ctx, nil); !isStatus(err, 403) || !strings.Contains(err.Error(), "no role") {
		t.Fatalf("carol, with no mapped group: %v", err)
	}
	// Unknown tokens, and a token too long to be a credential, are 401.
	if _, err = as("tok-mallory").Works(ctx, nil); !isStatus(err, 401) {
		t.Fatalf("an unknown token: %v", err)
	}

	// Verified once, then cached: alice's further calls don't ask again.
	before := idp.count()
	for range 3 {
		if _, err = alice.Works(ctx, url.Values{}); err != nil {
			t.Fatal(err)
		}
	}
	if idp.count() != before {
		t.Fatalf("asked the verifier %d more times for a cached credential", idp.count()-before)
	}
	// A verifier that fails now doesn't matter to a cached credential, and
	// fails a new one closed.
	idp.setFail(true)
	if _, err = alice.Works(ctx, nil); err != nil {
		t.Fatalf("a cached credential while the verifier fails: %v", err)
	}
	if _, err = as("tok-new").Works(ctx, nil); !isStatus(err, 401) {
		t.Fatalf("a failing verifier let a credential in: %v", err)
	}
	idp.setFail(false)

	// Changing the group table takes effect on the next request.
	if _, err = owner.SetIssuer(ctx, "corp", "fake", json.RawMessage(idpPeople), map[string]string{"everyone": "view"}); err != nil {
		t.Fatal(err)
	}
	if _, err = alice.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: "alice-2"}, "alice-2"); !isStatus(err, 403) {
		t.Fatalf("alice kept operate after the table changed: %v", err)
	}

	// The host stops accepting a credential at its expiry, cached or not.
	h.clock.Advance(11 * time.Minute)
	idp.setFail(true)
	if _, err = alice.Works(ctx, nil); !isStatus(err, 401) {
		t.Fatalf("an expired credential: %v", err)
	}
	idp.setFail(false)
	if _, err = alice.Works(ctx, nil); err != nil {
		t.Fatalf("alice's credential verified again: %v", err)
	}

	// Removing the issuer refuses its credentials from the next request.
	var issuers []client.Issuer
	h.mustCLI(&issuers, "auth", "issuers")
	if len(issuers) != 1 || issuers[0].ID != "corp" {
		t.Fatalf("issuers: %+v", issuers)
	}
	h.mustCLI(nil, "auth", "issuer", "remove", "corp")
	if _, err = alice.Works(ctx, nil); !isStatus(err, 401) {
		t.Fatalf("a removed issuer's credential: %v", err)
	}
	// The owner's own credential never depends on an issuer.
	if _, err = owner.Works(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// Each time a verifier is asked, the outcome is recorded: who it
	// named, or why it refused, never the token.
	verifications := map[string]int{}
	for _, r := range h.observed.named("auth.") {
		for _, v := range r.Attributes {
			if strings.Contains(v, "tok-") {
				t.Fatalf("a token reached the trail: %+v", r)
			}
		}
		verifications[r.Name+" "+r.Attributes["auth.issuer"]+" "+r.Attributes["auth.code"]+r.Attributes["operator.actor"]]++
	}
	for _, want := range []string{"auth.credential_verified corp operator:corp:alice@example.com", "auth.credential_refused corp refused", "auth.credential_refused corp verifier_failed"} {
		if verifications[want] == 0 {
			t.Errorf("no %q record: %v", want, verifications)
		}
	}

	// Each change is in the operator trail, refused or not.
	outcomes := map[string]int{}
	for _, r := range h.observed.named("operator.issuer") {
		outcomes[r.Name+" "+r.Attributes["operator.actor"]+" "+r.Attributes["operator.outcome"]]++
	}
	for _, want := range []string{"operator.issuer_set operator:local ok", "operator.issuer_set operator:local invalid_request", "operator.issuer_removed operator:local ok", "operator.issuer_set operator:local invalid_config"} {
		if outcomes[want] == 0 {
			t.Errorf("trail lacks %q: %v", want, outcomes)
		}
	}
}

// TestManagementIssuerSubjectsKeepTheirKeysApart: a subject may hold "/",
// so an issuer's operator's keys are namespaced by a digest of the
// subject. Before, operator:corp:x sending key y/k and operator:corp:x/y
// sending key k made the same delivery, and the second person's control
// came back as the first's. A long subject still fits a delivery ID.
func TestManagementIssuerSubjectsKeepTheirKeysApart(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	idp := &fakeIDP{now: h.clock.Now}
	h.verifiers = map[string]management.Verifier{"fake": {Verifier: idp}}
	long := strings.Repeat("s", 256)
	people := `{"people": {"tok-x": {"subject": "x", "groups": ["ops"]}, "tok-xy": {"subject": "x/y", "groups": ["ops"]}, "tok-long": {"subject": "` + long + `", "groups": ["ops"]}}}`
	owner := h.operatorClient()
	if _, err := owner.SetIssuer(ctx, "corp", "fake", json.RawMessage(people), map[string]string{"ops": "operate"}); err != nil {
		t.Fatal(err)
	}
	as := func(token string) *client.Client {
		c, err := client.New(h.endpoint, token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	work := func(delivery string) client.Work {
		sent, err := owner.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: delivery}, delivery)
		if err != nil {
			t.Fatal(err)
		}
		w, err := owner.Work(ctx, sent.RoutedWorkID)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	w1, w2, w3 := work("w1"), work("w2"), work("w3")
	first, err := as("tok-x").HoldWork(ctx, w1.ID, w1.Revision, "mine", "y/k")
	if err != nil {
		t.Fatal(err)
	}
	second, err := as("tok-xy").HoldWork(ctx, w2.ID, w2.Revision, "mine", "k")
	if err != nil {
		t.Fatal(err)
	}
	if second.EventID == first.EventID {
		t.Fatal("x/y's hold came back as x's")
	}
	e, err := owner.Event(ctx, second.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Actor != "operator:corp:x/y" || e.WorkID != w2.ID {
		t.Fatalf("x/y's hold: by %q on %s, want on %s", e.Actor, e.WorkID, w2.ID)
	}
	if _, err = as("tok-long").HoldWork(ctx, w3.ID, w3.Revision, "mine", strings.Repeat("k", 128)); err != nil {
		t.Fatalf("a 256-character subject with a 128-character key: %v", err)
	}
}
