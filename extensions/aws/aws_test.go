package aws_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/aws"
	"github.com/open-conduit/open-conduit/extensions/aws/awstest"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

const scope = aws.FixtureScope

var at = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

func now() time.Time { return at }

// harness configures the package against the fake STS with the fixture's
// entry: Work run on the helper binding, which may use both roles, and
// Work run on another binding, which may use neither.
func harness(t *testing.T, conn string, secrets bool) (*sdktest.Host, *awstest.Server) {
	t.Helper()
	fake := awstest.New(aws.FixtureRegion)
	fake.Now = now
	t.Cleanup(fake.Close)
	aws.SeedFixture(fake)
	if conn == "" {
		conn = `{"region":"` + aws.FixtureRegion + `","endpoint":"` + fake.URL + `"}`
	}
	names := []string{aws.SecretAccessKeyID, aws.SecretSecretAccessKey}
	if !secrets {
		names = nil
	}
	h := sdktest.New(t, aws.Build(aws.Options{Now: now, Ambient: func(string) func(context.Context) (aws.Keys, error) {
		return func(context.Context) (aws.Keys, error) { return aws.Keys{}, aws.ErrNoBaseIdentity }
	}}),
		sdktest.WithWorks(
			domain.Work{ID: "github/acme/infra/issue/12", ScopeID: scope, State: domain.WorkRunning},
			domain.Work{ID: "other", ScopeID: scope, State: domain.WorkRunning},
		),
		sdktest.WithExecutions(
			domain.Execution{ID: "github/acme/infra/issue/12/1", WorkID: "github/acme/infra/issue/12", ExecutorBinding: "helper", State: domain.ExecutionRunning},
			domain.Execution{ID: "other/1", WorkID: "other", ExecutorBinding: "reviewer", State: domain.ExecutionRunning},
		),
		sdktest.WithConnectionSecrets(scope, "aws-main", map[string]string{aws.SecretAccessKeyID: aws.FixtureKeyID, aws.SecretSecretAccessKey: aws.FixtureSecret}))
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: aws.Name, Name: "main",
		Config: aws.FixtureEntry(), Connection: &protocol.ConnectionEntry{ID: "aws-main", Type: aws.Name, Config: json.RawMessage(conn), Secrets: names}}}}); err != nil {
		t.Fatal(err)
	}
	return h, fake
}

// call makes the call as the run's profile does: with no request ID.
func call(t *testing.T, h *sdktest.Host, work, execution, input string, requestID string) (aws.Process, error) {
	t.Helper()
	var out protocol.OperationResult
	err := h.Call(context.Background(), "operation.call", protocol.OperationParams{Operation: aws.OpCredentials,
		Authority: protocol.FromAuthority(extension.Authority{ScopeID: scope, WorkID: domain.ID(work), ExecutionID: domain.ID(execution), ExpiresAt: at.Add(time.Hour), Operations: []string{aws.OpCredentials}}),
		Input:     protocol.Payload{Data: json.RawMessage(input)}, RequestID: requestID}, &out)
	if err != nil {
		return aws.Process{}, err
	}
	var p aws.Process
	if err := json.Unmarshal(out.Output.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p, nil
}

func code(err error) string {
	if c := protocol.ErrorCode(err); c != "" {
		return c
	}
	var op *extension.OperationError
	if errors.As(err, &op) {
		return op.Code
	}
	return ""
}

// The run gets a role's credentials in credential_process format, from
// sts:AssumeRole signed with the base key pair, for the role's duration,
// with the run's binding and Work in the session name; a role with
// tag_session also sends the scope, binding, Work and execution as tags.
// The issue is recorded without its credentials.
func TestCredentials(t *testing.T) {
	h, fake := harness(t, "", true)
	p, err := call(t, h, "github/acme/infra/issue/12", "github/acme/infra/issue/12/1", `{"role":"read"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || !strings.HasPrefix(p.AccessKeyID, "ASIA") || p.SecretAccessKey == "" || p.SessionToken == "" || p.Expiration != at.Add(15*time.Minute).Format(time.RFC3339) {
		t.Fatalf("credentials %+v", p)
	}
	w, err := call(t, h, "github/acme/infra/issue/12", "github/acme/infra/issue/12/1", `{"role":"write"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Expiration != at.Add(30*time.Minute).Format(time.RFC3339) {
		t.Fatalf("write role's duration: %s", w.Expiration)
	}
	got := fake.Assumed()
	if len(got) != 2 || got[0].RoleARN != aws.FixtureRead || got[0].SessionName != "oc.helper.github-acme-infra-issue-12" || got[0].Duration != 15*time.Minute || len(got[0].Tags) != 0 {
		t.Fatalf("assumed %+v", got)
	}
	if got[1].Tags[aws.TagBinding] != "helper" || got[1].Tags[aws.TagWork] != "github/acme/infra/issue/12" || got[1].Tags[aws.TagScope] != scope || got[1].Tags[aws.TagExecution] != "github/acme/infra/issue/12/1" {
		t.Fatalf("session tags %+v", got[1].Tags)
	}
	records := h.KV(aws.IssuedNS)
	if len(records) != 2 {
		t.Fatalf("records %v", records)
	}
	for _, raw := range records {
		s := string(raw)
		if strings.Contains(s, p.SecretAccessKey) || strings.Contains(s, p.SessionToken) || strings.Contains(s, w.SecretAccessKey) || strings.Contains(s, w.SessionToken) {
			t.Fatalf("a record holds a credential: %s", s)
		}
		var r aws.Record
		if err := json.Unmarshal(raw, &r); err != nil || r.Binding != "helper" || r.Work != "github/acme/infra/issue/12" || r.Account != aws.FixtureAccount || r.Expiration.IsZero() {
			t.Fatalf("record %s %v", s, err)
		}
	}
	captured := h.Captured()
	if len(captured) != 2 || captured[0].Name != aws.EventIssued {
		t.Fatalf("telemetry %+v", captured)
	}
	for _, c := range captured {
		b, _ := json.Marshal(c)
		if strings.Contains(string(b), p.SecretAccessKey) || strings.Contains(string(b), p.SessionToken) {
			t.Fatal("telemetry holds a credential")
		}
	}
}

// A run asks only for a role that names its binding, never one the scope
// lacks, and never with a request ID (its result would be kept for
// replay). A call from a run the scope doesn't hold is refused. None of
// them reaches STS.
func TestCredentialsRefused(t *testing.T) {
	h, fake := harness(t, "", true)
	for _, tc := range []struct {
		name, work, execution, input, request, want string
	}{
		{"another binding", "other", "other/1", `{"role":"read"}`, "", "role_not_granted"},
		{"an unknown role", "github/acme/infra/issue/12", "github/acme/infra/issue/12/1", `{"role":"admin"}`, "", "unknown_role"},
		{"no role", "github/acme/infra/issue/12", "github/acme/infra/issue/12/1", `{}`, "", "bad_request"},
		{"a request ID", "github/acme/infra/issue/12", "github/acme/infra/issue/12/1", `{"role":"read"}`, "r1", "request_id_refused"},
		{"another Work's execution", "other", "github/acme/infra/issue/12/1", `{"role":"read"}`, "", protocol.CodeUnauthorized},
		{"no such Work", "gone", "gone/1", `{"role":"read"}`, "", protocol.CodeUnauthorized},
	} {
		if _, err := call(t, h, tc.work, tc.execution, tc.input, tc.request); code(err) != tc.want {
			t.Errorf("%s: %v (%q), want %s", tc.name, err, code(err), tc.want)
		}
	}
	if n := len(fake.Assumed()); n != 0 {
		t.Fatalf("%d refused calls reached STS", n)
	}
	// Control: the same run asking for its own role gets it.
	if _, err := call(t, h, "github/acme/infra/issue/12", "github/acme/infra/issue/12/1", `{"role":"read"}`, ""); err != nil {
		t.Fatal(err)
	}
}

// STS refusing the role (its trust policy, a bad key) refuses the call
// with what to do, never STS's message; a connection with no key pair and
// no ambient identity says so.
func TestCredentialsSTSRefusal(t *testing.T) {
	h, fake := harness(t, "", true)
	fake.AddKey(aws.FixtureKeyID, "another-secret", aws.FixtureAccount, "arn:aws:iam::111122223333:user/x")
	if _, err := call(t, h, "github/acme/infra/issue/12", "github/acme/infra/issue/12/1", `{"role":"read"}`, ""); code(err) != "aws_refused" || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("a bad signature: %v", err)
	}
	ambient, _ := harness(t, "", false)
	if _, err := call(t, ambient, "github/acme/infra/issue/12", "github/acme/infra/issue/12/1", `{"role":"read"}`, ""); code(err) != "aws_credential" {
		t.Fatalf("no base identity: %v", err)
	}
}

// The run layer is an AWS config with a profile per role the binding may
// use, each asking the toolbelt, with the region and the connection's
// endpoint, and an empty shared credentials file; a binding with no role,
// or a run with no files directory, gets nothing.
func TestRunLayer(t *testing.T) {
	h, fake := harness(t, "", true)
	layer := func(binding, dir string) protocol.RunEnvironmentResult {
		var out protocol.RunEnvironmentResult
		if err := h.Call(context.Background(), "run.environment", protocol.RunEnvironmentParams{Owner: protocol.ResourceOwner{ScopeID: scope, WorkID: "github/acme/infra/issue/12", ExecutionID: "github/acme/infra/issue/12/1"},
			Binding: binding, FilesDir: dir}, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := layer("helper", "/run/x/token/files/aws")
	want := "[profile read]\ncredential_process = os call aws.credentials --output data --data '{\"role\":\"read\"}'\nregion = eu-west-2\nendpoint_url = " + fake.URL + "\n\n" +
		"[profile write]\ncredential_process = os call aws.credentials --output data --data '{\"role\":\"write\"}'\nregion = eu-west-2\nendpoint_url = " + fake.URL + "\n\n"
	if got.Files[aws.ConfigFile] != want {
		t.Fatalf("config:\n%s\nwant:\n%s", got.Files[aws.ConfigFile], want)
	}
	if c, ok := got.Files[aws.CredentialsFile]; !ok || c != "" {
		t.Fatalf("credentials file %q %v", c, ok)
	}
	if got.Env["AWS_CONFIG_FILE"] != "/run/x/token/files/aws/config" || got.Env["AWS_SHARED_CREDENTIALS_FILE"] != "/run/x/token/files/aws/credentials" {
		t.Fatalf("env %v", got.Env)
	}
	if extension.CheckRunFiles(got.Files) != nil {
		t.Fatal("the layer's files are not ones the host writes")
	}
	for _, empty := range []protocol.RunEnvironmentResult{layer("reviewer", "/run/x/token/files/aws"), layer("helper", "")} {
		if len(empty.Files) != 0 || len(empty.Env) != 0 {
			t.Fatalf("a run with no role or no files dir got %+v", empty)
		}
	}
}

// Doctor reads the base identity, and the deep check assumes each role.
func TestChecks(t *testing.T) {
	h, fake := harness(t, "", true)
	res, err := h.CheckRun(context.Background(), protocol.CheckRunParams{Check: aws.Name})
	if err != nil || !strings.Contains(res.Detail, "user/open-conduit") {
		t.Fatalf("check: %+v %v", res, err)
	}
	if res, err = h.CheckRun(context.Background(), protocol.CheckRunParams{Check: aws.Name + "-roles", Deep: true}); err != nil || !strings.Contains(res.Detail, "main read") || !strings.Contains(res.Detail, "main write") {
		t.Fatalf("deep check: %+v %v", res, err)
	}
	fake.AddKey(aws.FixtureKeyID, "rotated", aws.FixtureAccount, "x")
	if res, err = h.CheckRun(context.Background(), protocol.CheckRunParams{Check: aws.Name}); err == nil && res.OK {
		t.Fatalf("a base identity STS refuses passed: %+v", res)
	}
}

// Config: every role is in an allowed account, named once, for named
// bindings, lasting 15m to 12h; a connection has its region, and both
// halves of a key pair or neither.
func TestParse(t *testing.T) {
	if _, err := aws.ParseSettings(aws.FixtureEntry()); err != nil {
		t.Fatal(err)
	}
	role := func(r string) string {
		return `{"accounts":["111122223333"],"roles":[` + r + `]}`
	}
	for name, raw := range map[string]string{
		"another account":  role(`{"name":"read","arn":"arn:aws:iam::444455556666:role/x","for_bindings":["helper"]}`),
		"no accounts":      `{"roles":[{"name":"read","arn":"arn:aws:iam::111122223333:role/x","for_bindings":["helper"]}]}`,
		"a bad account":    `{"accounts":["1234"],"roles":[{"name":"read","arn":"arn:aws:iam::111122223333:role/x","for_bindings":["helper"]}]}`,
		"no roles":         `{"accounts":["111122223333"],"roles":[]}`,
		"not a role ARN":   role(`{"name":"read","arn":"arn:aws:iam::111122223333:user/x","for_bindings":["helper"]}`),
		"no bindings":      role(`{"name":"read","arn":"arn:aws:iam::111122223333:role/x"}`),
		"too short":        role(`{"name":"read","arn":"arn:aws:iam::111122223333:role/x","for_bindings":["h"],"duration":"5m"}`),
		"too long":         role(`{"name":"read","arn":"arn:aws:iam::111122223333:role/x","for_bindings":["h"],"duration":"13h"}`),
		"a name twice":     role(`{"name":"read","arn":"arn:aws:iam::111122223333:role/x","for_bindings":["h"]},{"name":"read","arn":"arn:aws:iam::111122223333:role/y","for_bindings":["h"]}`),
		"a bad name":       role(`{"name":"Read Only","arn":"arn:aws:iam::111122223333:role/x","for_bindings":["h"]}`),
		"an unknown field": role(`{"name":"read","arn":"arn:aws:iam::111122223333:role/x","for_bindings":["h"],"policy":"*"}`),
	} {
		if _, err := aws.ParseSettings(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	if _, err := aws.ParseConnection(json.RawMessage(`{"region":"eu-west-2","endpoint":"http://localhost:4566"}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"region":"Europe"}`, `{"region":"eu-west-2","endpoint":"localhost"}`} {
		if _, err := aws.ParseConnection(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted connection %s", raw)
		}
	}
	h, _ := harness(t, "", true)
	for _, names := range [][]string{{aws.SecretAccessKeyID}, {"token"}} {
		var out json.RawMessage
		if err := h.Call(context.Background(), "validate", protocol.ValidateParams{Kind: protocol.ValidateConnection, Type: aws.Name, Config: json.RawMessage(`{"region":"eu-west-2"}`), SecretNames: names}, &out); err == nil {
			t.Errorf("accepted connection secrets %v", names)
		}
	}
	var out protocol.ValidateResult
	if err := h.Call(context.Background(), "validate", protocol.ValidateParams{Kind: protocol.ValidateExtension, Type: aws.Name, Config: aws.FixtureEntry()}, &out); err != nil || len(out.Bindings) != 1 || out.Bindings[0] != "helper" {
		t.Fatalf("an entry's bindings: %+v %v", out, err)
	}
}

// A session name holds the binding and the Work in STS's characters and
// at most 64 of them, keeping a long Work ID's end after a digest of the
// whole, so two long IDs that share an end differ.
func TestSessionName(t *testing.T) {
	if got := aws.SessionName("helper", "github/acme/infra/issue/12"); got != "oc.helper.github-acme-infra-issue-12" {
		t.Fatal(got)
	}
	a := aws.SessionName("a-binding-with-a-very-long-name", domain.ID("linear/"+strings.Repeat("x", 80)+"/ENG-1"))
	b := aws.SessionName("a-binding-with-a-very-long-name", domain.ID("jira/"+strings.Repeat("x", 80)+"/ENG-1"))
	if len(a) > 64 || a == b || !strings.HasSuffix(a, "/ENG-1"[1:]) {
		t.Fatalf("%s %s", a, b)
	}
	for _, r := range a {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_+=,.@-", r)) {
			t.Fatalf("%q in %s", r, a)
		}
	}
}

// The ambient identity is the daemon's environment's key pair, else what
// the AWS CLI exports, kept until five minutes before it expires.
func TestAmbient(t *testing.T) {
	env := map[string]string{"AWS_ACCESS_KEY_ID": "AKIAENV", "AWS_SECRET_ACCESS_KEY": "s"}
	exports := 0
	clock := at
	a := &aws.Ambient{Getenv: func(k string) string { return env[k] }, Now: func() time.Time { return clock },
		Export: func(context.Context, string) ([]byte, error) {
			exports++
			return []byte(`{"Version":1,"AccessKeyId":"ASIACLI","SecretAccessKey":"s","SessionToken":"t","Expiration":"` + at.Add(time.Hour).Format(time.RFC3339) + `"}`), nil
		}}
	if k, err := a.Keys(context.Background()); err != nil || k.AccessKeyID != "AKIAENV" || exports != 0 {
		t.Fatalf("environment: %+v %v", k, err)
	}
	env = nil
	for i := 0; i < 2; i++ {
		if k, err := a.Keys(context.Background()); err != nil || k.AccessKeyID != "ASIACLI" || k.SessionToken != "t" {
			t.Fatalf("export: %+v %v", k, err)
		}
	}
	if exports != 1 {
		t.Fatalf("exported %d times before expiry", exports)
	}
	clock = at.Add(56 * time.Minute)
	if _, err := a.Keys(context.Background()); err != nil || exports != 2 {
		t.Fatalf("not exported again near expiry: %d %v", exports, err)
	}
	none := &aws.Ambient{Getenv: func(string) string { return "" }, Export: func(context.Context, string) ([]byte, error) { return nil, errors.New("no aws") }}
	if _, err := none.Keys(context.Background()); !errors.Is(err, aws.ErrNoBaseIdentity) {
		t.Fatalf("no identity: %v", err)
	}
}

// Records older than the retention are pruned; newer ones are kept.
func TestPrune(t *testing.T) {
	old := at.Add(-aws.RecordRetention-time.Hour).Format("20060102T150405.000000000Z") + "/x/1/ASIAOLD"
	fresh := at.Add(-time.Hour).Format("20060102T150405.000000000Z") + "/x/1/ASIANEW"
	h := sdktest.New(t, aws.Build(aws.Options{Now: now}), sdktest.WithKV(aws.IssuedNS, old, []byte(`{}`)), sdktest.WithKV(aws.IssuedNS, fresh, []byte(`{}`)))
	if err := h.RunTask(context.Background(), "aws-prune"); err != nil {
		t.Fatal(err)
	}
	kv := h.KV(aws.IssuedNS)
	if _, ok := kv[old]; ok || len(kv) != 1 {
		t.Fatalf("records after pruning: %v", kv)
	}
}
