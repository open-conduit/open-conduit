// Package client is the public local management API client and wire types.
// It dials only the configured Unix socket, ignores proxy environment
// variables and never follows redirects.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Payload struct {
	Schema string          `json:"schema,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}
type Key struct {
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
}
type Work struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	ScopeID         string   `json:"scope_id"`
	State           string   `json:"state"`
	CorrelationKeys []Key    `json:"correlation_keys"`
	Context         *Payload `json:"context,omitempty"`
	ParentID        string   `json:"parent_id,omitempty"`
	Revision        string   `json:"revision"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	// Hold and Annotations are present on GET /work/{id} only.
	Hold        *Hold        `json:"hold,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

// Hold is an operator scheduling hold on Work.
type Hold struct {
	EventID string `json:"event_id"`
	Reason  string `json:"reason,omitempty"`
	At      string `json:"at"`
}

// Annotation is extension-contributed, display-only Work context.
type Annotation struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}
type Event struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Source         string   `json:"source"`
	DeliveryID     string   `json:"delivery_id"`
	OccurredAt     string   `json:"occurred_at"`
	ReceivedAt     string   `json:"received_at"`
	CorrelationKey *Key     `json:"correlation_key,omitempty"`
	CausationID    string   `json:"causation_id,omitempty"`
	WorkID         string   `json:"work_id,omitempty"`
	ExecutionID    string   `json:"execution_id,omitempty"`
	Data           *Payload `json:"data,omitempty"`
	ScopeID        string   `json:"scope_id,omitempty"`
	Sequence       string   `json:"sequence"`
	Routed         bool     `json:"routed"`
	RoutedWorkID   string   `json:"routed_work_id,omitempty"`
	Disposition    string   `json:"disposition,omitempty"`
}
type Outcome struct {
	Kind    string   `json:"kind"`
	Summary string   `json:"summary,omitempty"`
	Data    *Payload `json:"data,omitempty"`
}
type Artifact struct {
	ID          string `json:"id"`
	Size        int64  `json:"size"`
	MediaType   string `json:"media_type,omitempty"`
	WorkID      string `json:"work_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
}
type Lease struct {
	Owner     string `json:"owner,omitempty"`
	Epoch     string `json:"epoch"`
	ExpiresAt string `json:"expires_at,omitempty"`
}
type Execution struct {
	ID              string     `json:"id"`
	WorkID          string     `json:"work_id"`
	ScopeID         string     `json:"scope_id,omitempty"`
	ExecutorBinding string     `json:"executor_binding"`
	Attempt         uint32     `json:"attempt"`
	State           string     `json:"state"`
	TriggerID       string     `json:"trigger_id"`
	Lease           Lease      `json:"lease"`
	Outcome         *Outcome   `json:"outcome,omitempty"`
	Artifacts       []Artifact `json:"artifacts"`
	TraceID         string     `json:"trace_id,omitempty"`
	StartedAt       string     `json:"started_at,omitempty"`
	FinishedAt      string     `json:"finished_at,omitempty"`
	Revision        string     `json:"revision"`
	CreatedAt       string     `json:"created_at"`
	UpdatedAt       string     `json:"updated_at"`
}
type Match struct {
	Keys  []Key    `json:"keys"`
	Types []string `json:"types"`
}
type Wait struct {
	ID             string `json:"id"`
	WorkID         string `json:"work_id"`
	ExecutionID    string `json:"execution_id"`
	State          string `json:"state"`
	Match          Match  `json:"match"`
	TimeoutTimerID string `json:"timeout_timer_id,omitempty"`
	AfterSequence  string `json:"after_sequence"`
	WakeEventID    string `json:"wake_event_id,omitempty"`
	Revision       string `json:"revision"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}
type Requires struct {
	Interrupt bool   `json:"interrupt"`
	Inject    bool   `json:"inject"`
	Resume    bool   `json:"resume"`
	Workspace bool   `json:"workspace"`
	Toolbelt  string `json:"toolbelt,omitempty"`
}
type Binding struct {
	Name     string   `json:"name"`
	Executor string   `json:"executor"`
	Requires Requires `json:"requires"`
}
type Route struct {
	Name         string `json:"name"`
	Handler      string `json:"handler"`
	Binding      string `json:"binding"`
	ActivePolicy string `json:"active_policy"`
}
type Scope struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	ParentID   string            `json:"parent_id,omitempty"`
	Enabled    bool              `json:"enabled"`
	Bindings   []Binding         `json:"bindings"`
	Routes     []Route           `json:"routes"`
	Config     json.RawMessage   `json:"config,omitempty"`
	SecretRefs map[string]string `json:"secret_refs"`
	Revision   string            `json:"revision"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
}
type Health struct {
	State            string            `json:"state"`
	Version          string            `json:"version"`
	InstanceID       string            `json:"instance_id"`
	ConfigGeneration string            `json:"config_generation"`
	Components       map[string]string `json:"components"`
}
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
}
type ControlResult struct {
	EventID     string `json:"event_id"`
	WorkID      string `json:"work_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
}
type Detail struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error is a safe server error envelope.
type Error struct {
	Status    int      `json:"-"`
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	RequestID string   `json:"request_id"`
	Details   []Detail `json:"details,omitempty"`
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
	for _, d := range e.Details {
		s += fmt.Sprintf("\n  %s: %s: %s", d.Path, d.Code, d.Message)
	}
	return s
}

// ErrNotModified reports a 304 for a conditional read.
var ErrNotModified = errors.New("not modified")

// ErrUnreachable wraps a failure to reach the management endpoint at all
// (no socket, nothing listening), as opposed to an error the daemon returned.
var ErrUnreachable = errors.New("management endpoint unreachable")

type Client struct {
	http  *http.Client
	token string
}

// ParseEndpoint accepts only unix:///absolute/path.
func ParseEndpoint(endpoint string) (string, error) {
	path, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("endpoint must be unix:///absolute/path")
	}
	return path, nil
}

// New builds a client for one endpoint and bearer token.
func New(endpoint, token string) (*Client, error) {
	socket, err := ParseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("missing bearer token")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
	}
	return &Client{http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, token: token}, nil
}

// DiscoverEndpoint applies --endpoint → OPEN_CONDUIT_ENDPOINT → socket
// discovery (--socket → OPEN_CONDUIT_SOCKET → <state>/management.sock).
func DiscoverEndpoint(flag, socket, stateDir string, env func(string) string) string {
	if flag != "" {
		return flag
	}
	if e := env("OPEN_CONDUIT_ENDPOINT"); e != "" {
		return e
	}
	if socket == "" {
		socket = env("OPEN_CONDUIT_SOCKET")
	}
	if socket == "" {
		socket = filepath.Join(stateDir, "management.sock")
	}
	return "unix://" + socket
}

// TokenFile is the private operator credential shape.
type TokenFile struct {
	TokenID string `json:"token_id"`
	Token   string `json:"token"`
}

// ReadTokenFile refuses group/world-readable or non-regular credential files.
func ReadTokenFile(path string) (TokenFile, error) {
	var f TokenFile
	info, err := os.Lstat(path)
	if err != nil {
		return f, fmt.Errorf("operator token file unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return f, fmt.Errorf("operator token file must be a private 0600 regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return f, fmt.Errorf("operator token file unreadable")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&f); err != nil || f.Token == "" || f.TokenID == "" {
		return TokenFile{}, fmt.Errorf("operator token file is malformed")
	}
	return f, nil
}

// Do performs one request and decodes the data envelope into out.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string, out any) error {
	return c.do(ctx, method, path, query, body, idempotencyKey, nil, out)
}
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, key string, header http.Header, out any) error {
	u := "http://localhost/v1/" + strings.TrimPrefix(path, "/")
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return ErrNotModified
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var env struct {
			Error Error `json:"error"`
		}
		if json.Unmarshal(data, &env) != nil || env.Error.Code == "" {
			return &Error{Status: resp.StatusCode, Code: "http_error", Message: http.StatusText(resp.StatusCode)}
		}
		env.Error.Status = resp.StatusCode
		return &env.Error
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(data, &env); err != nil {
		return err
	}
	return json.Unmarshal(env.Data, out)
}

func (c *Client) Health(ctx context.Context) (h Health, err error) {
	err = c.Do(ctx, "GET", "health", nil, nil, "", &h)
	return
}
func get[T any](c *Client, ctx context.Context, path string) (out T, err error) {
	err = c.Do(ctx, "GET", path, nil, nil, "", &out)
	return
}
func list[T any](c *Client, ctx context.Context, path string, q url.Values) (out Page[T], err error) {
	err = c.Do(ctx, "GET", path, q, nil, "", &out)
	return
}
func esc(id string) string { return url.PathEscape(id) }

func (c *Client) Scopes(ctx context.Context, q url.Values) (Page[Scope], error) {
	return list[Scope](c, ctx, "scopes", q)
}
func (c *Client) Scope(ctx context.Context, id string) (Scope, error) {
	return get[Scope](c, ctx, "scopes/"+esc(id))
}
func (c *Client) Works(ctx context.Context, q url.Values) (Page[Work], error) {
	return list[Work](c, ctx, "work", q)
}
func (c *Client) Work(ctx context.Context, id string) (Work, error) {
	return get[Work](c, ctx, "work/"+esc(id))
}
func (c *Client) Events(ctx context.Context, q url.Values) (Page[Event], error) {
	return list[Event](c, ctx, "events", q)
}
func (c *Client) Event(ctx context.Context, id string) (Event, error) {
	return get[Event](c, ctx, "events/"+esc(id))
}

// EventRoute explains how an accepted Event was routed: its disposition and
// the durable route decision.
func (c *Client) EventRoute(ctx context.Context, id string) (EventRoute, error) {
	return get[EventRoute](c, ctx, "events/"+esc(id)+"/route")
}

// EventRoute is an Event's routing explanation.
type EventRoute struct {
	EventID      string `json:"event_id"`
	Type         string `json:"type"`
	ScopeID      string `json:"scope_id,omitempty"`
	Routed       bool   `json:"routed"`
	Disposition  string `json:"disposition,omitempty"`
	RoutedWorkID string `json:"routed_work_id,omitempty"`
	Route        string `json:"route,omitempty"`
	Binding      string `json:"binding,omitempty"`
	Generation   string `json:"generation,omitempty"`
	Policy       string `json:"policy,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func (c *Client) Executions(ctx context.Context, q url.Values) (Page[Execution], error) {
	return list[Execution](c, ctx, "executions", q)
}
func (c *Client) Execution(ctx context.Context, id string) (Execution, error) {
	return get[Execution](c, ctx, "executions/"+esc(id))
}
func (c *Client) Waits(ctx context.Context, q url.Values) (Page[Wait], error) {
	return list[Wait](c, ctx, "waits", q)
}
func (c *Client) Wait(ctx context.Context, id string) (Wait, error) {
	return get[Wait](c, ctx, "waits/"+esc(id))
}
func (c *Client) Artifacts(ctx context.Context, q url.Values) (Page[Artifact], error) {
	return list[Artifact](c, ctx, "artifacts", q)
}

// ArtifactContent streams one artifact, optionally with a Range header value.
func (c *Client) ArtifactContent(ctx context.Context, id, byteRange string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/artifacts/"+esc(id)+"/content", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var env struct {
			Error Error `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		env.Error.Status = resp.StatusCode
		return &env.Error
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// EventChain walks causation links from id to its root (inclusive), newest first.
func (c *Client) EventChain(ctx context.Context, id string) ([]Event, error) {
	var chain []Event
	for id != "" && len(chain) < 1024 {
		e, err := c.Event(ctx, id)
		if err != nil {
			return chain, err
		}
		chain = append(chain, e)
		id = e.CausationID
	}
	return chain, nil
}

type EventRequest struct {
	ScopeID        string   `json:"scope_id"`
	Type           string   `json:"type"`
	DeliveryID     string   `json:"delivery_id"`
	Data           *Payload `json:"data,omitempty"`
	CorrelationKey *Key     `json:"correlation_key,omitempty"`
	WorkID         string   `json:"work_id,omitempty"`
	CausationID    string   `json:"causation_id,omitempty"`
}

func (c *Client) SubmitEvent(ctx context.Context, r EventRequest, key string) (e Event, err error) {
	err = c.Do(ctx, "POST", "events", nil, r, key, &e)
	return
}

type WorkRequest struct {
	ScopeID         string   `json:"scope_id"`
	Kind            string   `json:"kind"`
	Context         *Payload `json:"context,omitempty"`
	CorrelationKeys []Key    `json:"correlation_keys,omitempty"`
	ParentID        string   `json:"parent_id,omitempty"`
	Binding         string   `json:"binding"`
}
type WorkCreated struct {
	Work    Work   `json:"work"`
	EventID string `json:"event_id"`
}

func (c *Client) CreateWork(ctx context.Context, r WorkRequest, key string) (out WorkCreated, err error) {
	err = c.Do(ctx, "POST", "work", nil, r, key, &out)
	return
}

type controlRequest struct {
	ExpectedRevision string `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (c *Client) CancelWork(ctx context.Context, id, revision, reason, key string) (out ControlResult, err error) {
	err = c.Do(ctx, "POST", "work/"+esc(id)+"/cancel", nil, controlRequest{revision, reason}, key, &out)
	return
}
func (c *Client) RetryWork(ctx context.Context, id, revision, reason, key string) (out ControlResult, err error) {
	err = c.Do(ctx, "POST", "work/"+esc(id)+"/retry", nil, controlRequest{revision, reason}, key, &out)
	return
}
func (c *Client) CancelExecution(ctx context.Context, id, revision, reason, key string) (out ControlResult, err error) {
	err = c.Do(ctx, "POST", "executions/"+esc(id)+"/cancel", nil, controlRequest{revision, reason}, key, &out)
	return
}

// RebindWork moves ready Work's queued input to another binding of its scope.
func (c *Client) RebindWork(ctx context.Context, id, revision, binding, reason, key string) (out ControlResult, err error) {
	body := struct {
		ExpectedRevision string `json:"expected_revision"`
		Binding          string `json:"binding"`
		Reason           string `json:"reason"`
	}{revision, binding, reason}
	err = c.Do(ctx, "POST", "work/"+esc(id)+"/rebind", nil, body, key, &out)
	return
}

// HoldWork stops attempts starting or resuming for Work until released.
func (c *Client) HoldWork(ctx context.Context, id, revision, reason, key string) (out ControlResult, err error) {
	err = c.Do(ctx, "POST", "work/"+esc(id)+"/hold", nil, controlRequest{revision, reason}, key, &out)
	return
}

// ReleaseWork removes an operator hold.
func (c *Client) ReleaseWork(ctx context.Context, id, revision, reason, key string) (out ControlResult, err error) {
	err = c.Do(ctx, "POST", "work/"+esc(id)+"/release", nil, controlRequest{revision, reason}, key, &out)
	return
}

// Action is an action an extension offers on a scope or Work now.
// Provider and ID address it; Prompt, when set, asks for one line of input.
type Action struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	Prompt   string `json:"prompt,omitempty"`
}

// ActionTarget names what actions apply to: a scope, or Work.
type ActionTarget struct{ ScopeID, WorkID string }

func (t ActionTarget) path() string { return t.base() + "/actions" }

func (t ActionTarget) base() string {
	if t.WorkID != "" {
		return "work/" + esc(t.WorkID)
	}
	return "scopes/" + esc(t.ScopeID)
}

// ActionResult is an invoked action's accepted Event and the provider's
// message.
type ActionResult struct {
	Event   Event  `json:"event"`
	Message string `json:"message,omitempty"`
}

// Actions lists the actions available on a target now.
func (c *Client) Actions(ctx context.Context, t ActionTarget) ([]Action, error) {
	var out struct {
		Actions []Action `json:"actions"`
	}
	err := c.Do(ctx, "GET", t.path(), nil, nil, "", &out)
	return out.Actions, err
}

// InvokeAction invokes one action as the local operator. A refusal is an
// *Error carrying the provider's code and message.
func (c *Client) InvokeAction(ctx context.Context, t ActionTarget, provider, id, input, key string) (out ActionResult, err error) {
	err = c.Do(ctx, "POST", t.path()+"/"+esc(provider)+"/"+esc(id), nil, map[string]string{"input": input}, key, &out)
	return
}

// Panel is a panel an extension shows on a scope or Work: a constrained
// document of sections that a view renders as it is. A section's Actions
// are the provider's actions listed on the target now, invoked with
// InvokeAction like any other.
type Panel struct {
	Provider string         `json:"provider"`
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Sections []PanelSection `json:"sections"`
}

type PanelSection struct {
	Title   string       `json:"title,omitempty"`
	Text    string       `json:"text,omitempty"`
	Fields  []PanelField `json:"fields,omitempty"`
	Table   *PanelTable  `json:"table,omitempty"`
	Links   []PanelLink  `json:"links,omitempty"`
	Actions []Action     `json:"actions,omitempty"`
}

type PanelField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type PanelTable struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

type PanelLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Panels lists the panels extensions show on a target now.
func (c *Client) Panels(ctx context.Context, t ActionTarget) ([]Panel, error) {
	var out struct {
		Panels []Panel `json:"panels"`
	}
	err := c.Do(ctx, "GET", t.base()+"/panels", nil, nil, "", &out)
	return out.Panels, err
}

// AttachSpec is the local command that attaches to a Work's session.
type AttachSpec struct {
	WorkID   string   `json:"work_id"`
	Executor string   `json:"executor"`
	Argv     []string `json:"argv"`
	Dir      string   `json:"dir,omitempty"`
}

// AttachWork returns the attach command for held Work.
func (c *Client) AttachWork(ctx context.Context, id, key string) (out AttachSpec, err error) {
	err = c.Do(ctx, "POST", "work/"+esc(id)+"/attach", nil, struct{}{}, key, &out)
	return
}

// ReconcileResult names the ingress sources made due.
type ReconcileResult struct {
	EventID string   `json:"event_id"`
	ScopeID string   `json:"scope_id"`
	Sources []string `json:"sources"`
}

// ReconcileScope requests an immediate reconcile of a scope's sources.
func (c *Client) ReconcileScope(ctx context.Context, id, revision, key string) (out ReconcileResult, err error) {
	body := struct {
		ExpectedRevision string `json:"expected_revision"`
	}{revision}
	err = c.Do(ctx, "POST", "scopes/"+esc(id)+"/reconcile", nil, body, key, &out)
	return
}

type WakeRequest struct {
	ExpectedRevision string   `json:"expected_revision"`
	Type             string   `json:"type"`
	DeliveryID       string   `json:"delivery_id"`
	Data             *Payload `json:"data,omitempty"`
	CorrelationKey   *Key     `json:"correlation_key"`
}
type WakeResult struct {
	Event   Event `json:"event"`
	Matched bool  `json:"matched"`
}

func (c *Client) Wake(ctx context.Context, id string, r WakeRequest, key string) (out WakeResult, err error) {
	err = c.Do(ctx, "POST", "work/"+esc(id)+"/wake", nil, r, key, &out)
	return
}

type ConfigValidation struct {
	Valid         bool            `json:"valid"`
	CandidateHash string          `json:"candidate_hash"`
	Config        json.RawMessage `json:"config"`
}
type ConfigApplied struct {
	Generation string `json:"generation"`
	Hash       string `json:"hash"`
}

// Config is the active generation's wiring: each scope's extensions,
// routes and bindings, and the connections they use.
type Config struct {
	Generation  string             `json:"generation"`
	Hash        string             `json:"hash"`
	Connections []ConfigConnection `json:"connections"`
	Scopes      []ConfigScope      `json:"scopes"`
}
type ConfigConnection struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}
type ConfigScope struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	ParentID   string            `json:"parent_id,omitempty"`
	Enabled    bool              `json:"enabled"`
	Extensions []ConfigExtension `json:"extensions"`
	Routes     []Route           `json:"routes"`
	Bindings   []ConfigBinding   `json:"bindings"`
}

// ConfigExtension is an extension a scope runs; Ingress ones bring events in.
type ConfigExtension struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Connection string `json:"connection,omitempty"`
	Ingress    bool   `json:"ingress"`
}

// ConfigBinding names an executor and the connection and resource it uses.
type ConfigBinding struct {
	Name       string `json:"name"`
	Executor   string `json:"executor"`
	Connection string `json:"connection,omitempty"`
	Resource   string `json:"resource,omitempty"`
}

func (c *Client) Config(ctx context.Context) (Config, error) {
	return get[Config](c, ctx, "config")
}
func (c *Client) ValidateConfig(ctx context.Context, config json.RawMessage) (out ConfigValidation, err error) {
	err = c.Do(ctx, "POST", "config/validate", nil, map[string]json.RawMessage{"config": config}, "", &out)
	return
}
func (c *Client) ApplyConfig(ctx context.Context, config json.RawMessage, hash, generation, key string) (out ConfigApplied, err error) {
	err = c.Do(ctx, "POST", "config/apply", nil, map[string]any{"config": config, "candidate_hash": hash, "expected_generation": generation}, key, &out)
	return
}

// Rotate returns the one-time replacement credential.
func (c *Client) Rotate(ctx context.Context, expectedTokenID string) (out TokenFile, err error) {
	err = c.Do(ctx, "POST", "auth/rotate", nil, map[string]string{"expected_token_id": expectedTokenID}, "", &out)
	return
}

type Revocation struct {
	TokenID   string `json:"token_id"`
	RevokedAt string `json:"revoked_at"`
}

func (c *Client) Revoke(ctx context.Context, tokenID, reason, key string) (out Revocation, err error) {
	err = c.Do(ctx, "POST", "auth/revoke", nil, map[string]string{"token_id": tokenID, "reason": reason}, key, &out)
	return
}

// ToolCall is the execution toolbelt request; input is operation-specific JSON.
type ToolCall struct {
	Operation   string          `json:"operation"`
	Input       json.RawMessage `json:"input,omitempty"`
	RequestID   string          `json:"request_id,omitempty"`
	ScopeID     string          `json:"scope_id,omitempty"`
	WorkID      string          `json:"work_id,omitempty"`
	ExecutionID string          `json:"execution_id,omitempty"`
}
type ToolResult struct {
	Event   *Event   `json:"event,omitempty"`
	Work    *Work    `json:"work,omitempty"`
	Wait    *Wait    `json:"wait,omitempty"`
	Context *Payload `json:"context,omitempty"`
	Output  *Payload `json:"output,omitempty"`
}

func (c *Client) ToolbeltCall(ctx context.Context, call ToolCall) (out ToolResult, err error) {
	err = c.Do(ctx, "POST", "toolbelt/call", nil, call, "", &out)
	return
}

// ---- unified run history and doctor ----

type Trigger struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	OccurredAt  string `json:"occurred_at,omitempty"`
	CausationID string `json:"causation_id,omitempty"`
}
type Usage struct {
	InputTokens  *uint64  `json:"input_tokens,omitempty"`
	OutputTokens *uint64  `json:"output_tokens,omitempty"`
	CachedTokens *uint64  `json:"cached_tokens,omitempty"`
	Cost         *float64 `json:"cost,omitempty"`
	Currency     string   `json:"currency,omitempty"`
}
type RetryStatus struct {
	FailureCount  int    `json:"failure_count"`
	EligibleAt    string `json:"eligible_at,omitempty"`
	Blocked       bool   `json:"blocked"`
	Exhausted     bool   `json:"exhausted"`
	BlockedReason string `json:"blocked_reason,omitempty"`
}
type Retention struct {
	State           string `json:"state"`
	ExpiredAt       string `json:"expired_at,omitempty"`
	ContentReleased bool   `json:"content_released"`
}

// Run is one attempt in the unified history, identical for every executor.
type Run struct {
	Execution
	WorkKind                string        `json:"work_kind"`
	WorkState               string        `json:"work_state"`
	Trigger                 *Trigger      `json:"trigger,omitempty"`
	CurrentTrigger          *Trigger      `json:"current_trigger,omitempty"`
	Executor                string        `json:"executor,omitempty"`
	ConfigGeneration        string        `json:"config_generation,omitempty"`
	LaunchState             string        `json:"launch_state,omitempty"`
	ResourceExcluded        bool          `json:"resource_excluded"`
	Deadline                string        `json:"deadline,omitempty"`
	RemainingTimeoutSeconds int64         `json:"remaining_timeout_seconds,omitempty"`
	DurationMS              int64         `json:"duration_ms,omitempty"`
	Usage                   *Usage        `json:"usage,omitempty"`
	Workspace               *RunWorkspace `json:"workspace,omitempty"`
	Inputs                  int           `json:"inputs"`
	WaitID                  string        `json:"wait_id,omitempty"`
	WakeEventID             string        `json:"wake_event_id,omitempty"`
	Retry                   *RetryStatus  `json:"retry,omitempty"`
	Retention               Retention     `json:"retention"`
}

// RunWorkspace is the resource a run's executor reported it ran in.
type RunWorkspace struct {
	Kind    string `json:"kind,omitempty"`
	Ref     string `json:"ref"`
	Dir     string `json:"dir,omitempty"`
	Summary string `json:"summary,omitempty"`
}
type Check struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Code   string   `json:"code"`
	Detail string   `json:"detail,omitempty"`
	Items  []string `json:"items,omitempty"`
}
type Doctor struct {
	State  string  `json:"state"`
	Checks []Check `json:"checks"`
}

// Runs lists the unified history. Filters: scope_id, work_id, state,
// executor_binding, executor, work_kind, outcome, limit, cursor.
func (c *Client) Runs(ctx context.Context, q url.Values) (Page[Run], error) {
	return list[Run](c, ctx, "runs", q)
}
func (c *Client) Run(ctx context.Context, id string) (Run, error) {
	return get[Run](c, ctx, "runs/"+esc(id))
}

// LiveOutput is one output of a running run, as far as it has got.
type LiveOutput struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size"`
	Content   string `json:"content"`
}

// LiveRun is what a running run has written so far.
type LiveRun struct {
	RunID   string       `json:"run_id"`
	Outputs []LiveOutput `json:"outputs"`
}

// RunLive returns a running run's output so far, each output at most its
// last tail bytes (the server's default when tail is not positive). A run
// that is not running answers 409 not_live; its output is in its artifacts.
func (c *Client) RunLive(ctx context.Context, id string, tail int64) (out LiveRun, err error) {
	q := url.Values{}
	if tail > 0 {
		q.Set("tail", strconv.FormatInt(tail, 10))
	}
	err = c.Do(ctx, "GET", "runs/"+esc(id)+"/live", q, nil, "", &out)
	return
}

// IsNotLive reports a RunLive answer that the run is not running.
func IsNotLive(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "not_live"
}

// Doctor runs health checks; checkSecrets explicitly resolves references.
func (c *Client) Doctor(ctx context.Context, checkSecrets bool) (out Doctor, err error) {
	q := url.Values{}
	if checkSecrets {
		q.Set("check_secrets", "true")
	}
	err = c.Do(ctx, "GET", "doctor", q, nil, "", &out)
	return
}
