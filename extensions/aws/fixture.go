package aws

import (
	"encoding/json"
	"io"

	"github.com/open-conduit/open-conduit/extensions/aws/awstest"
	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json: a fixture command
// that serves the fake STS for the run.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(Name), "certify-fixture")}
}

// Fixture identities: the fake's base key pair, its account and roles.
const (
	FixtureScope   = "infra"
	FixtureRegion  = "eu-west-2"
	FixtureAccount = "111122223333"
	FixtureKeyID   = "AKIAFIXTURE"
	FixtureSecret  = "fixture-secret-key"
	FixtureRead    = "arn:aws:iam::111122223333:role/agent-read"
	FixtureWrite   = "arn:aws:iam::111122223333:role/agent-write"
)

// FixtureEntry is the aws entry config the fixture configures.
func FixtureEntry() json.RawMessage {
	return json.RawMessage(`{"accounts":["` + FixtureAccount + `"],"roles":[` +
		`{"name":"read","arn":"` + FixtureRead + `","for_bindings":["helper"]},` +
		`{"name":"write","arn":"` + FixtureWrite + `","for_bindings":["helper"],"duration":"30m","tag_session":true}]}`)
}

// SeedFixture lets the fake's base key pair assume both roles; the write
// role's trust policy allows session tags.
func SeedFixture(fake *awstest.Server) {
	fake.AddKey(FixtureKeyID, FixtureSecret, FixtureAccount, "arn:aws:iam::"+FixtureAccount+":user/open-conduit")
	fake.AddRole(FixtureRead, false)
	fake.AddRole(FixtureWrite, true)
}

// NewFixture builds the certification fixture against a running fake. The
// operation cases bind the calling run from durable state, so a call from
// no run the scope holds is refused, and a call must name a role.
func NewFixture(fake *awstest.Server) protocol.Fixture {
	SeedFixture(fake)
	conn := json.RawMessage(`{"region":"` + FixtureRegion + `","endpoint":"` + fake.URL + `"}`)
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: FixtureScope, Type: Name, Name: "main", Config: FixtureEntry(),
			Connection: &protocol.ConnectionEntry{ID: "aws-main", Type: Name, Config: conn, Secrets: []string{SecretAccessKeyID, SecretSecretAccessKey}}}},
		Secrets: map[string]string{SecretAccessKeyID: FixtureKeyID, SecretSecretAccessKey: FixtureSecret},
		Operations: []protocol.OperationCase{
			{Operation: OpCredentials, Input: protocol.Payload{Schema: "aws.credentials.request.v1", Data: json.RawMessage(`{}`)}, Error: "bad_request"},
			{Operation: OpCredentials, Input: protocol.Payload{Schema: "aws.credentials.request.v1", Data: json.RawMessage(`{"role":"read"}`)}, Error: protocol.CodeUnauthorized},
		},
	}
}

// ServeFixture runs the certification fixture command: it starts the fake
// STS, prints the fixture as one JSON line and serves until stdin closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	fake := awstest.New(FixtureRegion)
	defer fake.Close()
	b, err := json.Marshal(NewFixture(fake))
	if err != nil {
		return err
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, stdin)
	return nil
}
