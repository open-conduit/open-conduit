package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// OpCredentials is the operation that issues a role's credentials.
const OpCredentials = "aws.credentials"

// IssuedNS keeps a record of each credential issue (never the
// credentials): binding, Work, execution, role, account, session and
// expiry, by time. Records older than RecordRetention are pruned.
const IssuedNS = "aws.issued"

// RecordRetention is how long an issue's record is kept.
const RecordRetention = 90 * 24 * time.Hour

// EventIssued is the telemetry event each credential issue records.
const EventIssued = "aws_credentials_issued"

// Session tag keys, sent when a role sets tag_session.
const (
	TagScope     = "open-conduit:scope"
	TagBinding   = "open-conduit:binding"
	TagWork      = "open-conduit:work"
	TagExecution = "open-conduit:execution"
)

type tools struct{ st *state }

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func asSTS(err error, target **STSError) bool { return errors.As(err, target) }

// CredentialsInput is aws.credentials' input.
type CredentialsInput struct {
	// Role is the name of a role the run's binding may use.
	Role string `json:"role" doc:"the AWS role to use: a profile name from aws configure list-profiles, such as read"`
}

// Record is what IssuedNS keeps of an issue.
type Record struct {
	Scope       string    `json:"scope"`
	Work        string    `json:"work"`
	Execution   string    `json:"execution"`
	Binding     string    `json:"binding"`
	Entry       string    `json:"entry"`
	Role        string    `json:"role"`
	RoleARN     string    `json:"role_arn"`
	Account     string    `json:"account"`
	SessionName string    `json:"session_name"`
	AccessKeyID string    `json:"access_key_id"`
	IssuedAt    time.Time `json:"issued_at"`
	Expiration  time.Time `json:"expiration"`
	Tagged      bool      `json:"tagged,omitempty"`
}

func (t tools) register(ext *sdk.Extension) {
	ext.Operation(OpCredentials, sdk.Handler(t.credentials),
		// The host scrubs these from whatever the run publishes, should an
		// agent run the profile's command by hand.
		sdk.SecretOutput("SecretAccessKey", "SessionToken"),
		sdk.Describe("Short-lived AWS credentials for one of the roles this run may use, in credential_process format."),
		sdk.Guidance("Your AWS CLI and SDKs already have a profile for each AWS role you may use, so run them with the profile: `aws --profile read s3 ls`, or set `AWS_PROFILE=read` for an SDK. `aws configure list-profiles` lists them. Each profile asks Open Conduit for short-lived credentials when it needs them and again when they expire, and CloudTrail records every call under this run, so you never handle a key and never call this operation yourself. If a command fails saying the call is held, a person is deciding whether this run may use that role: end your turn as your instructions say, and run the command again once you are resumed. Never print, copy or store credentials."))
	ext.Task("aws-prune", 24*time.Hour, t.prune)
}

// caller is the calling run as stored: its Work and its binding.
func (t tools) caller(ctx context.Context, a extension.Authority) (string, error) {
	var w domain.Work
	var x domain.Execution
	err := storage.Retry(sdk.NewStore(sdk.HostFrom(ctx)), storage.DefaultRetry).Transact(ctx, storage.Guard{Now: t.st.o.Now()}, func(tx storage.Tx) error {
		var err error
		if w, err = tx.Work(a.WorkID); err != nil {
			return err
		}
		x, err = tx.Execution(a.ExecutionID)
		return err
	})
	if err != nil || w.ScopeID != a.ScopeID || x.WorkID != a.WorkID || x.ExecutorBinding == "" {
		return "", extension.ErrUnauthorized
	}
	return x.ExecutorBinding, nil
}

func (t tools) credentials(ctx context.Context, a extension.Authority, in CredentialsInput) (Process, error) {
	if in.Role == "" {
		return Process{}, refuse("bad_request", "role names the AWS role to use, a profile's name")
	}
	binding, err := t.caller(ctx, a)
	if err != nil {
		return Process{}, err
	}
	// A call with a request ID would keep its result, the credentials,
	// for replay. The run's profiles never send one.
	if extension.CallRequestID(ctx) != "" {
		return Process{}, refuse("request_id_refused", "%s takes no request_id: its result is a credential and is never kept", OpCredentials)
	}
	var e *entry
	var role Role
	var mine []string
	for _, x := range t.st.inScope(a.ScopeID) {
		for _, r := range x.settings.RolesFor(binding) {
			mine = append(mine, r.Name)
		}
		if r, ok := x.settings.Role(in.Role); ok && e == nil {
			e, role = x, r
		}
	}
	slices.Sort(mine)
	which := "this run may use no AWS role"
	if len(mine) > 0 {
		which = "this run may use: " + strings.Join(mine, ", ")
	}
	if e == nil {
		return Process{}, refuse("unknown_role", "no AWS role %q in this scope; %s", in.Role, which)
	}
	if !slices.Contains(role.ForBindings, binding) {
		return Process{}, refuse("role_not_granted", "role %q is not for this run's binding; %s", in.Role, which)
	}
	// The account allow-list holds at the call as at validation.
	account := AccountOf(role.ARN)
	if account == "" || !slices.Contains(e.settings.Accounts, account) {
		return Process{}, refuse("account_not_allowed", "role %q is in an account the entry doesn't allow", in.Role)
	}
	req := AssumeRequest{RoleARN: role.ARN, SessionName: SessionName(binding, a.WorkID), Duration: role.Lifetime(), ExternalID: role.ExternalID}
	if role.TagSession {
		req.Tags = map[string]string{TagScope: tagValue(string(a.ScopeID)), TagBinding: tagValue(binding), TagWork: tagValue(string(a.WorkID)), TagExecution: tagValue(string(a.ExecutionID))}
	}
	now := t.st.o.Now()
	c, err := e.sts.AssumeRole(ctx, req)
	if err != nil {
		return Process{}, refusal(in.Role, err)
	}
	// STS answered for the role asked for, in its account: anything else
	// is not handed to the run.
	if parts := strings.Split(c.AssumedRoleARN, ":"); len(parts) < 6 || parts[0] != "arn" || parts[4] != account {
		return Process{}, refuse("aws_refused", "STS answered for another account than role %q's; report it as a problem", in.Role)
	}
	rec := Record{Scope: string(a.ScopeID), Work: string(a.WorkID), Execution: string(a.ExecutionID), Binding: binding, Entry: e.name, Role: role.Name, RoleARN: role.ARN,
		Account: account, SessionName: req.SessionName, AccessKeyID: c.AccessKeyID, IssuedAt: now.UTC(), Expiration: c.Expiration, Tagged: role.TagSession}
	// No credential leaves without its record: the record is what maps
	// the access key ID CloudTrail shows back to this run. Unrecorded, the
	// session just expires unused.
	if err := t.record(ctx, rec); err != nil {
		return Process{}, refuse("aws_unavailable", "the credentials for role %q could not be recorded, so they were not handed out; try again", in.Role)
	}
	return Process{Version: 1, AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Expiration: c.Expiration.Format(time.RFC3339)}, nil
}

// record leaves the issue's trail: a durable record, which must be
// written, and a telemetry event linked to the Work, which is best-effort.
// Neither holds a credential; the access key ID is what CloudTrail shows
// beside the session name.
func (t tools) record(ctx context.Context, r Record) error {
	h := sdk.HostFrom(ctx)
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	key := r.IssuedAt.Format("20060102T150405.000000000Z") + "/" + r.Execution + "/" + r.AccessKeyID
	if err := h.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{{Namespace: IssuedNS, Key: key, Value: raw}}}); err != nil {
		return err
	}
	_ = h.Capture(ctx, domain.ID(r.Scope), EventIssued, r.IssuedAt, map[string]any{
		"work_id": r.Work, "execution_id": r.Execution, "binding": r.Binding, "role": r.Role, "account": r.Account,
		"session_name": r.SessionName, "access_key_id": r.AccessKeyID, "expiration": r.Expiration.Format(time.RFC3339), "tagged": r.Tagged})
	return nil
}

// prune deletes the records older than RecordRetention.
func (t tools) prune(ctx context.Context, h *sdk.Host) error {
	cutoff := t.st.o.Now().Add(-RecordRetention).UTC().Format("20060102T150405.000000000Z")
	entries, err := h.List(ctx, IssuedNS, "")
	if err != nil {
		return err
	}
	var del []protocol.KVDelete
	for _, e := range entries {
		if e.Key < cutoff {
			del = append(del, protocol.KVDelete{Namespace: IssuedNS, Key: e.Key})
		}
	}
	if len(del) == 0 {
		return nil
	}
	return h.Commit(ctx, protocol.KVCommitParams{Deletes: del})
}

// refusal maps an STS failure onto what the agent can act on, never a
// credential or STS's message.
func refusal(role string, err error) error {
	var se *STSError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, ErrNoBaseIdentity):
		return refuse("aws_credential", "Open Conduit has no AWS identity to assume role %q with; report it as a problem", role)
	case errors.As(err, &se) && (se.Code == "AccessDenied" || se.Status == 403):
		return refuse("aws_refused", "AWS refused to let Open Conduit assume role %q (%s); report it as a problem", role, describe(err))
	case errors.As(err, &se):
		return refuse("aws_refused", "AWS refused the credentials for role %q (%s); report it as a problem", role, describe(err))
	}
	return refuse("aws_unavailable", "AWS STS could not be reached for role %q; try again", role)
}

// SessionName is the role session name CloudTrail records every call
// under: "oc.<binding>.<work>", in STS's characters and at most 64 of them.
// A Work ID too long to fit keeps its end, after a short digest of the
// whole, so two runs never share a name; the issue's record maps it back.
func SessionName(binding string, work domain.ID) string {
	b := sessionChars(binding)
	if len(b) > 20 {
		b = b[:20]
	}
	w := sessionChars(string(work))
	room := 64 - len("oc.") - len(b) - 1
	if len(w) > room {
		sum := sha256.Sum256([]byte(work))
		tail := room - 9
		w = hex.EncodeToString(sum[:])[:8] + "." + w[len(w)-tail:]
	}
	return "oc." + b + "." + w
}

// sessionChars replaces what a role session name can't hold ([\w+=,.@-]).
func sessionChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_+=,.@-", r) {
			return r
		}
		return '-'
	}, s)
}

// tagValue is a session tag value: at most 256 of the characters a tag
// may hold.
func tagValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" _.:/=+-@", r) {
			return r
		}
		return '-'
	}, s)
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}
