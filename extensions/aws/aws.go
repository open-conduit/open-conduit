// Package aws gives a run short-lived AWS credentials for a named IAM role,
// from the host, when it asks: the model AWS itself uses for an ECS task or
// a Lambda function. The daemon holds one base identity per connection (a
// key pair in the connection's secrets, or its own ambient identity), and
// an entry names the roles a scope's bindings may assume, each in an
// account the entry allows. The aws.credentials operation assumes a role
// with sts:AssumeRole, naming the run's binding and Work in the session
// (so CloudTrail shows which run made each call), and answers in the
// credential_process format every AWS SDK reads. The run layer writes an
// AWS config file with one profile per role the run's binding may use, whose
// credential_process is `os call aws.credentials`, so `aws --profile read`
// works with no step by the agent. A person can gate a role with the human
// extension's gate on the operation's input.
//
// It is an installed package on protocol/sdk alone, so a third party could
// have written it.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Name is the package's name, its extension and connection type.
const Name = "aws"

// Version is this package build's own version.
const Version = "0.1.0"

// ConfigFile and CredentialsFile are the run files the layer writes: the
// AWS config with a profile per role, and an empty shared credentials
// file, so a profile of the same name in the operator's own
// ~/.aws/credentials can't stand in for one of the run's.
const (
	ConfigFile      = "config"
	CredentialsFile = "credentials"
)

// Options configure Build.
type Options struct {
	Now  func() time.Time
	HTTP *http.Client
	// Ambient replaces the daemon's ambient identity (tests).
	Ambient func(profile string) func(context.Context) (Keys, error)
}

type entry struct {
	name, scope, config string
	conn                Connection
	connection          string
	settings            Settings
	sts                 *STS
}

type state struct {
	o       Options
	host    atomic.Pointer[sdk.Host]
	mu      sync.Mutex
	entries map[string]*entry // by scope/name
}

func (st *state) list() []*entry {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*entry, 0, len(st.entries))
	for _, e := range st.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

// inScope are a scope's entries, in name order.
func (st *state) inScope(scope domain.ID) []*entry {
	var out []*entry
	for _, e := range st.list() {
		if e.scope == string(scope) {
			out = append(out, e)
		}
	}
	return out
}

// Build returns the served extension.
func Build(o Options) *sdk.Extension {
	if o.Now == nil {
		o.Now = time.Now
	}
	st := &state{o: o, entries: map[string]*entry{}}
	ext := sdk.New(Name, sdk.Version(Version), sdk.Command(protocol.HostRunCommand(Name)...),
		sdk.Permissions(protocol.PermissionTelemetryCapture),
		sdk.Summary("Short-lived AWS credentials for a named IAM role, issued by the host to a run when it asks, with the run named in CloudTrail."))
	ext.Manifest.Title = "AWS"
	ext.Manifest.Provides.Connections = []protocol.TypeDecl{{Type: Name, Fields: []protocol.FieldDecl{
		{Name: "region", Help: "the AWS region STS is called in, which every profile the run gets uses too, such as eu-west-2", Required: true},
		{Name: "endpoint", Help: "an endpoint the host calls instead of STS's regional one (an STS VPC endpoint, an emulator); the run never calls it"},
		{Name: "run_endpoint", Help: "an endpoint the run's profiles send every AWS call to (endpoint_url), for an emulator such as LocalStack that serves every service at one address; leave it unset for real AWS"},
		{Name: "profile", Help: "the AWS CLI profile that gives the daemon's own identity when the connection has no key pair (default: the AWS CLI's default chain)"},
		{Name: SecretAccessKeyID, Help: "the base identity's access key ID (with secret_access_key); without both, the daemon's own AWS identity", Secret: true},
		{Name: SecretSecretAccessKey, Help: "the base identity's secret access key", Secret: true},
	}}}
	ext.Manifest.Provides.Extensions = []protocol.ExtensionDecl{{Type: Name, Fields: []protocol.FieldDecl{
		{Name: "accounts", Help: "the 12-digit AWS account IDs a role may be in; a role in any other account is refused, at validation and at the call", Required: true},
		{Name: "roles", Help: `[{"name": "read", "arn": "arn:aws:iam::111122223333:role/agent-read", "for_bindings": ["helper"], "duration": "15m", "tag_session": false, "external_id": ""}]: the roles a run may get credentials for, each only for the bindings it names; duration from 15m (the default) to 12h; tag_session sends the scope, binding and Work as session tags, which the role's trust policy must allow (sts:TagSession)`, Required: true},
	}}}
	ext.Manifest.Provides.RunEnvironment = true
	tools{st}.register(ext)
	ext.Check(Name, st.check)
	ext.Check(Name+"-roles", st.checkRoles, sdk.Deep())
	ext.Validate = func(_ context.Context, p protocol.ValidateParams) error {
		switch p.Kind {
		case protocol.ValidateConnection:
			if _, err := ParseConnection(p.Config); err != nil {
				return extension.Invalid("", "invalid_aws_connection", err.Error())
			}
			return checkConnectionSecrets(p.SecretNames)
		case protocol.ValidateExtension:
			if len(p.SecretNames) > 0 {
				return extension.Invalid("/secret_names", "aws_takes_no_secrets", "an aws entry takes no secrets; the base key pair goes in its connection's secret_refs")
			}
			if _, err := ParseSettings(p.Config); err != nil {
				return extension.Invalid("/config", "invalid_aws_entry", err.Error())
			}
		}
		return nil
	}
	ext.Bindings = func(p protocol.ValidateParams) []string {
		s, err := ParseSettings(p.Config)
		if err != nil {
			return nil
		}
		return s.Bindings()
	}
	ext.Configure = st.configure
	ext.RunLayer = st.runLayer
	return ext
}

// Manifest is the package manifest.
func Manifest() protocol.Manifest { return Build(Options{}).Manifest }

// checkConnectionSecrets: a connection names both halves of a key pair,
// or neither (the daemon's ambient identity).
func checkConnectionSecrets(names []string) error {
	if len(names) == 0 {
		return nil
	}
	for _, n := range names {
		if n != SecretAccessKeyID && n != SecretSecretAccessKey {
			return extension.Invalid("/secret_refs", "unknown_aws_secret", "an aws connection's secrets are access_key_id and secret_access_key")
		}
	}
	if !slices.Contains(names, SecretAccessKeyID) || !slices.Contains(names, SecretSecretAccessKey) {
		return extension.Invalid("/secret_refs", "aws_key_pair", "an aws connection needs both access_key_id and secret_access_key, or neither for the daemon's own identity")
	}
	return nil
}

// configure applies one generation; an unchanged entry keeps its STS
// client (and its cached ambient identity).
func (st *state) configure(_ context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	var out protocol.ConfigureResult
	st.host.Store(h)
	st.mu.Lock()
	prev := st.entries
	st.mu.Unlock()
	next := map[string]*entry{}
	roles := map[string]string{} // scope/role -> entry
	for _, e := range p.Entries {
		if e.Type != Name {
			continue
		}
		settings, err := ParseSettings(e.Config)
		if err != nil {
			return out, protocol.Errorf(protocol.CodeInvalid, "invalid aws entry")
		}
		if e.Connection == nil || e.Connection.Type != Name {
			return out, protocol.Errorf(protocol.CodeInvalid, "an aws entry needs an aws connection")
		}
		conn, err := ParseConnection(e.Connection.Config)
		if err != nil || checkConnectionSecrets(e.Connection.Secrets) != nil {
			return out, protocol.Errorf(protocol.CodeInvalid, "invalid aws connection")
		}
		label := e.Name
		if label == "" {
			label = "entry"
		}
		for _, r := range settings.Roles {
			key := e.ScopeID + "/" + r.Name
			if other, ok := roles[key]; ok && other != label {
				return out, protocol.Errorf(protocol.CodeInvalid, "two aws entries of one scope name the same role")
			}
			roles[key] = label
		}
		name := e.ScopeID + "/" + label
		digest, _ := json.Marshal(e)
		if old, ok := prev[name]; ok && old.config == string(digest) {
			next[name] = old
			continue
		}
		scope, connID, hasKeys := domain.ID(e.ScopeID), e.Connection.ID, len(e.Connection.Secrets) > 0
		var keys func(context.Context) (Keys, error)
		switch {
		case hasKeys:
			keys = func(ctx context.Context) (Keys, error) {
				values, err := st.host.Load().ConnectionSecrets(ctx, scope, connID, SecretAccessKeyID, SecretSecretAccessKey)
				if err != nil || values[SecretAccessKeyID] == "" || values[SecretSecretAccessKey] == "" {
					return Keys{}, ErrNoBaseIdentity
				}
				return Keys{AccessKeyID: values[SecretAccessKeyID], SecretAccessKey: values[SecretSecretAccessKey]}, nil
			}
		case st.o.Ambient != nil:
			keys = st.o.Ambient(conn.Profile)
		default:
			keys = (&Ambient{Profile: conn.Profile, Now: st.o.Now}).Keys
		}
		next[name] = &entry{name: name, scope: e.ScopeID, config: string(digest), conn: conn, connection: connID, settings: settings,
			sts: &STS{Region: conn.Region, Endpoint: conn.Endpoint, Keys: keys, HTTP: st.o.HTTP, Now: st.o.Now}}
	}
	st.mu.Lock()
	st.entries = next
	st.mu.Unlock()
	return out, nil
}

// runLayer is the run's AWS config: a profile for each role the run's
// binding may ask for, whose credential_process asks the toolbelt, with
// the connection's region and, when it sets one, its run endpoint. A run whose
// binding may use no role, or that has no directory for files (no
// toolbelt, so nothing could answer the profiles), gets nothing.
func (st *state) runLayer(_ context.Context, _ *sdk.Host, r extension.RunEnvironmentRequest) (extension.RunLayer, error) {
	if r.FilesDir == "" || r.Binding == "" {
		return extension.RunLayer{}, nil
	}
	var b strings.Builder
	n := 0
	for _, e := range st.inScope(r.Owner.ScopeID) {
		for _, role := range e.settings.RolesFor(r.Binding) {
			n++
			fmt.Fprintf(&b, "[profile %s]\ncredential_process = %s\nregion = %s\n", role.Name, CredentialProcess(role.Name), e.conn.Region)
			if e.conn.RunEndpoint != "" {
				fmt.Fprintf(&b, "endpoint_url = %s\n", e.conn.RunEndpoint)
			}
			b.WriteString("\n")
		}
	}
	if n == 0 {
		return extension.RunLayer{}, nil
	}
	return extension.RunLayer{
		Files: map[string]string{ConfigFile: b.String(), CredentialsFile: ""},
		Env: map[string]string{
			"AWS_CONFIG_FILE":             filepath.Join(r.FilesDir, ConfigFile),
			"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(r.FilesDir, CredentialsFile),
			// The v1 Go SDK reads the config file only when told to.
			"AWS_SDK_LOAD_CONFIG": "1",
		},
	}, nil
}

// CredentialProcess is a role's profile's credential_process: the
// toolbelt call whose output data is the credentials. Every AWS SDK runs
// it through a shell or splits it as one, so the input is quoted for both.
func CredentialProcess(role string) string {
	return "os call " + OpCredentials + ` --output data --data '{"role":"` + role + `"}'`
}

// check is the doctor check: each entry's base identity is valid
// (sts:GetCallerIdentity).
func (st *state) check(ctx context.Context, _ *sdk.Host, _ bool) (string, error) {
	entries := st.list()
	if len(entries) == 0 {
		return "no AWS entries are configured", nil
	}
	var items []string
	for _, e := range entries {
		id, err := e.sts.CallerIdentity(ctx)
		if err != nil {
			return "", fmt.Errorf("%s: the base identity of connection %s can't be used: %s", e.name, e.connection, describe(err))
		}
		items = append(items, fmt.Sprintf("%s: base identity %s, %d roles", e.name, id.ARN, len(e.settings.Roles)))
	}
	return strings.Join(items, "; "), nil
}

// checkRoles is the deep doctor check: the base identity can assume each
// role, for the shortest session STS issues.
func (st *state) checkRoles(ctx context.Context, _ *sdk.Host, deep bool) (string, error) {
	if !deep {
		return "run doctor --deep to assume each role", nil
	}
	var items, failed []string
	for _, e := range st.list() {
		for _, r := range e.settings.Roles {
			_, err := e.sts.AssumeRole(ctx, AssumeRequest{RoleARN: r.ARN, SessionName: "open-conduit-doctor", Duration: MinDuration, ExternalID: r.ExternalID})
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s role %s: %s", e.name, r.Name, describe(err)))
				continue
			}
			items = append(items, e.name+" "+r.Name)
		}
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	if len(items) == 0 {
		return "no AWS roles are configured", nil
	}
	return "assumed " + strings.Join(items, ", "), nil
}

// describe is a failure as the operator can act on it, never with a
// credential or a raw service body.
func describe(err error) string {
	var se *STSError
	switch {
	case err == ErrNoBaseIdentity:
		return "no base identity (set the connection's access_key_id and secret_access_key, or give the daemon an AWS identity)"
	case asSTS(err, &se) && se.Code != "":
		return "STS refused with " + se.Code
	case asSTS(err, &se):
		return fmt.Sprintf("STS answered HTTP %d", se.Status)
	}
	return "STS could not be reached"
}
