// Package tracker is the shared kit of the first-party issue-tracker
// extensions (06-01): Linear and Jira. Each tracker supplies a small Service
// (its API client and webhook format); the kit supplies everything that is
// the same for every tracker and runs on public contracts only:
//
//   - an ingress pair (push and reconcile) normalizing issues and comments into
//     canonical observations, with reopen generations;
//   - a deterministic route handler holding the opt-in/skip claim policy, an
//     optional attribute match for multi-scope routing, and input selection;
//   - a projector converging claim/status labels and workflow transitions from
//     durable Work state, and publishing marked comments exactly once;
//   - the agent-facing tool operations (context, issue view/comment/list/create).
//
// Labels and workflow statuses are projections: the runtime never reads them
// as state. Tracker-specific markers (Linear's visible footer, Jira's comment
// entity property) and transitions stay behind the Service boundary.
package tracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Issue is one tracker issue as the Service reads it.
type Issue struct {
	// Key is the tracker's issue key ("ENG-12", "PROJ-7").
	Key    string
	Title  string
	Body   string
	URL    string
	Author string
	Labels []string
	Open   bool
	// Waiting marks an open issue outside the states the entry claims from
	// (Linear's backlog/triage): observed, so closes and comments still reach
	// an owner, but never claimed on its own.
	Waiting bool
	// Status is the workflow state's name ("In Progress").
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Attrs are the properties routes can match on (team, project, priority,
	// assignee, components, custom fields), lowercased names.
	Attrs map[string][]string
}

// Agent marks a comment written by an execution or the projector. Work and
// Intent come from Open Conduit's mark; From is the issue key Open Supervisor
// put in its mark, kept for fallback parity.
type Agent struct {
	Work   string `json:"work,omitempty"`
	Intent string `json:"intent,omitempty"`
	From   string `json:"from,omitempty"`
}

// Comment is one comment on an issue.
type Comment struct {
	ID     string
	Body   string
	URL    string
	Author string
	Bot    bool
	// Agent is set when the comment carries an agent mark (or was written by
	// the connection's configured agent account).
	Agent     *Agent
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Mark is what the Service writes into a comment so it is recognized as an
// agent's (and, with Intent, so an ambiguous post can be found again).
type Mark struct {
	Work, Intent string
	// From is the issue key the agent works on (Open Supervisor's mark).
	From string
}

// Posted is a published comment.
type Posted struct {
	ID, URL string
}

// NewIssue is an issue to open.
type NewIssue struct {
	Title, Body string
	Labels      []string
}

// IssuePage is one page of a listing in a stable order.
type IssuePage struct {
	Issues []Issue
	// Next continues the listing; "" means the listing is complete.
	Next string
}

// Hook is a verified webhook delivery: what it is about, so Push can read
// the current state back exactly as reconcile would.
type Hook struct {
	// ID is the delivery's stable identity (the service's delivery header,
	// else a digest of the body).
	ID string `json:"id"`
	// Event names the delivery ("Issue:update", "comment_created").
	Event string `json:"event"`
	// Issues are issue keys whose state the delivery reports.
	Issues []string `json:"issues,omitempty"`
	// Comments are comments the delivery reports.
	Comments []CommentRef `json:"comments,omitempty"`
}

// CommentRef names a comment; Issue may be empty when the delivery does not
// say (Linear's comment deliveries carry only the comment's id).
type CommentRef struct {
	Issue string `json:"issue,omitempty"`
	ID    string `json:"id"`
}

// Service is one tracker account bound to one entry's issues. It performs
// no coordination: identity, generations, projections and intents belong to
// the kit. Implementations must never put credentials into errors.
type Service interface {
	// Verify proves the credential works and says who it is ("signed in as …").
	Verify(ctx context.Context) (string, error)
	// ListIssues returns one page of the entry's issues in a stable order
	// (creation order): every open issue and every issue closed or updated
	// since closedSince.
	ListIssues(ctx context.Context, closedSince time.Time, cursor string) (IssuePage, error)
	// Issue reads one issue. ErrNotFound when it is gone.
	Issue(ctx context.Context, key string) (Issue, error)
	// Comments lists every comment on an issue, oldest first.
	Comments(ctx context.Context, key string) ([]Comment, error)
	// ReadComment reads one comment and the key of the issue it is on.
	ReadComment(ctx context.Context, ref CommentRef) (string, Comment, error)
	// Reopens lists every time the issue left a closed state.
	Reopens(ctx context.Context, key string) ([]time.Time, error)
	// EditLabels adds and removes labels in one go.
	EditLabels(ctx context.Context, key string, add, remove []string) error
	// Transition moves the issue to the named workflow status (a status or
	// transition name configured by the operator). Already there is a no-op.
	Transition(ctx context.Context, key, status string) error
	// PostComment posts a comment carrying the mark.
	PostComment(ctx context.Context, key, body string, m Mark) (Posted, error)
	// CreateIssue opens an issue with its labels already on.
	CreateIssue(ctx context.Context, n NewIssue) (Issue, error)
	// Owns reports whether an issue key belongs to the entry.
	Owns(key string) bool
	// IssueURL is the issue's page.
	IssueURL(key string) string
	// RefFor is how a pull request body refers to the issue so the tracker
	// links (and maybe closes) it.
	RefFor(key string) string
	// ParseHook authenticates a raw webhook request and says what it is
	// about. ErrSignature for a bad or missing signature.
	ParseHook(h http.Header, body []byte, secret string, now time.Time) (Hook, error)
	// EnsureWebhook points the service's hook at url, creating it if need
	// be, and returns its id.
	EnsureWebhook(ctx context.Context, url, secret, existing string) (string, error)
}

// Errors the kit and Services share.
var (
	// ErrCredential means a credential could not be resolved or was refused.
	ErrCredential = errors.New("tracker credential unavailable")
	// ErrNotFound means the item is gone (or the connection cannot see it).
	ErrNotFound = errors.New("tracker item not found")
	// ErrSignature means a webhook delivery failed verification; its body is
	// never parsed or retained.
	ErrSignature = errors.New("tracker delivery signature rejected")
	// ErrIntentConflict means a stable intent ID was reused for different content.
	ErrIntentConflict = errors.New("tracker intent reused with different content")
)

// StatusError is a service answer with an unexpected HTTP status. It never
// carries the response body.
type StatusError struct {
	Status int
}

func (e *StatusError) Error() string { return fmt.Sprintf("tracker service answered %d", e.Status) }

// IsStatus reports whether err is a StatusError with that status.
func IsStatus(err error, status int) bool {
	var s *StatusError
	return errors.As(err, &s) && s.Status == status
}

// RateLimitError means the service asked us to wait until Until.
type RateLimitError struct{ Until time.Time }

func (e *RateLimitError) Error() string {
	return "tracker rate limited until " + e.Until.UTC().Format(time.RFC3339)
}

// SecretFunc resolves a named connection secret lazily.
type SecretFunc func(ctx context.Context, name string) (string, error)

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Digest is the kit's stable digest (exported for Services' delivery IDs).
func Digest(parts ...string) string { return digest(parts...) }

// ErrorCode maps a failure onto a safe diagnostic code.
func ErrorCode(err error) string {
	var s *StatusError
	var limit *RateLimitError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &limit):
		return "rate_limited"
	case errors.Is(err, ErrCredential):
		return "credential"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.As(err, &s):
		return fmt.Sprintf("http_%d", s.Status)
	case errors.Is(err, ErrIntentConflict):
		return "intent_conflict"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	return "failed"
}
