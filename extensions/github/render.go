package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// RenderEvent renders one GitHub Event the way an agent reads it at the
// start of a turn (Open Supervisor's core.Render), with each comment's
// reply hint naming the tool that answers it. ok is false for Events this
// extension does not own; the host falls back to the generic renderer.
//
// A render states what happened and never what to do about it: the same
// text reaches every Work subscribed to the item, whatever its role (the
// issue's author, a PR's reviewer), and the response belongs to that role's
// playbook. Pointers to the operation that shows more (`github.checks` for
// failed checks, the reply operation for a comment) are information.
func RenderEvent(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeCommentHuman, TypeCommentAgent, TypeCommentBot:
		var c CommentPayload
		if e.Data.Schema != SchemaComment || json.Unmarshal(e.Data.Data, &c) != nil {
			return "", false
		}
		return renderComment(c), true
	case TypeChecksPassed, TypeChecksFailed, TypeChecksNone, TypeChecksMissing:
		var c ChecksPayload
		if e.Data.Schema != SchemaChecks || json.Unmarshal(e.Data.Data, &c) != nil {
			return "", false
		}
		switch e.Type {
		case TypeChecksNone:
			return fmt.Sprintf("### No checks ran on PR #%d at %s\nThe repo has no CI or deploy previews.", c.Number, short(c.SHA)), true
		case TypeChecksPassed:
			s := fmt.Sprintf("### Checks passed on PR #%d at %s", c.Number, short(c.SHA))
			if c.Preview != "" {
				s += "\nPreview: " + c.Preview
			}
			return s, true
		case TypeChecksMissing:
			return fmt.Sprintf("### Required checks never reported on PR #%d at %s: %s\n%s", c.Number, short(c.SHA), strings.Join(c.Missing, ", "), missingFact), true
		}
		s := fmt.Sprintf("### Checks failed on PR #%d at %s: %s\nSee `call %s {\"number\": %d}` for links to each.", c.Number, short(c.SHA), strings.Join(c.Failed, ", "), OpChecks, c.Number)
		if len(c.Missing) > 0 {
			s += fmt.Sprintf("\nThese required checks never reported either: %s. %s", strings.Join(c.Missing, ", "), missingFact)
		}
		return s, true
	case TypePullConflicts:
		var c ConflictsPayload
		if e.Data.Schema != SchemaConflict || json.Unmarshal(e.Data.Data, &c) != nil {
			return "", false
		}
		return fmt.Sprintf("### PR #%d has conflicts with %s at %s\nIt can't merge until they're resolved.", c.Number, c.BaseRef, short(c.HeadSHA)), true
	case TypePullOpen:
		var p PullPayload
		if e.Data.Schema != SchemaPull || json.Unmarshal(e.Data.Data, &p) != nil {
			return "", false
		}
		return renderPullOpen(p), true
	case TypePullMerged, TypePullClosed:
		var p PullPayload
		if e.Data.Schema != SchemaPull || json.Unmarshal(e.Data.Data, &p) != nil {
			return "", false
		}
		action := "closed"
		if e.Type == TypePullMerged {
			action = "merged"
		}
		return fmt.Sprintf("### PR #%d %s", p.Number, action), true
	case TypeIssueClosed:
		return "### The issue was closed", true
	case TypeClaimRequested:
		n := 0
		if e.CorrelationKey != nil {
			n, _, _ = ParseIssueKey(e.CorrelationKey.ExternalID)
		}
		return fmt.Sprintf("### Claimed by hand\nYour operator claimed issue #%d for you, whatever its labels; `call %s` shows it.", n, OpContext), true
	case TypeDependencyMet:
		var p struct {
			SourceType string `json:"source_type"`
			Label      string `json:"label"`
		}
		_ = json.Unmarshal(e.Data.Data, &p)
		if p.Label == peerLabel {
			return "### What you were waiting for\nAnother agent commented while you waited; its comment is with this turn. What you were waiting for may not have happened yet.", true
		}
		c, err := ParseCond(p.Label)
		if err != nil {
			return "### What you were waiting for\nWhat you were waiting for has happened.", true
		}
		return fmt.Sprintf("### What you were waiting for\nYou were waiting for %s. That has happened: %s. Anything else you were waiting for may not have.", c.Describe(), c.Happened(p.SourceType)), true
	case "core.wait.timeout":
		return "### What you were waiting for\nIt is now past the latest time you said to wait until, and none of what you waited for has happened.", true
	case TypeHoldReleased:
		var p ReleasedPayload
		_ = json.Unmarshal(e.Data.Data, &p)
		return "### What you were waiting for\n" + strings.TrimSpace(p.Why), true
	}
	return "", false
}

// missingFact follows the required checks that never reported.
const missingFact = "The PR can't merge without them."

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// renderPullOpen renders an open PR's state. The same Event type carries
// the PR's opening and every later version of its payload (a push, or a
// new title, body, labels, base branch or draft state), so it leads with the head and says the PR may have
// changed rather than reading as a repeat of the opening.
func renderPullOpen(p PullPayload) string {
	head := fmt.Sprintf("### PR #%d is open", p.Number)
	if p.Draft {
		head = fmt.Sprintf("### PR #%d is open as a draft", p.Number)
	}
	if p.HeadSHA != "" {
		head += " at " + short(p.HeadSHA)
	}
	lines := []string{head}
	opened := "Opened"
	if p.Author != "" {
		opened += " by " + p.Author
	}
	if !p.CreatedAt.IsZero() {
		opened += " at " + p.CreatedAt.UTC().Format(time.RFC3339)
	}
	if opened != "Opened" {
		lines = append(lines, opened)
	}
	if p.Title != "" {
		lines = append(lines, "Title: "+p.Title)
	}
	if p.URL != "" {
		lines = append(lines, p.URL)
	}
	lines = append(lines, "This comes when the PR opens and again whenever anything about it changes: a push, or its title, body, labels, base branch or draft state. If its head isn't the one you last saw, it has new commits.")
	out := strings.Join(lines, "\n")
	if body := strings.TrimSpace(p.Body); body != "" {
		out += "\n\n" + body
	}
	return out
}

func renderComment(c CommentPayload) string {
	var where, reply string
	switch c.Kind {
	case KindIssue:
		where = "Comment on the issue"
		reply = fmt.Sprintf("reply with `call %s {\"body\": …}`", OpIssueComment)
	case KindPullRequest:
		where = fmt.Sprintf("Comment on PR #%d", c.Number)
		reply = fmt.Sprintf("reply with `call %s {\"number\": %d, \"body\": …}`", OpPRComment, c.Number)
	case KindReviewComment:
		where = fmt.Sprintf("Inline comment on PR #%d", c.Number)
		if c.Path != "" {
			where += " at " + c.Path
			if c.Line > 0 {
				where += fmt.Sprintf(":%d", c.Line)
			}
		}
		reply = fmt.Sprintf("reply with `call %s {\"number\": %d, \"comment_id\": %d, \"body\": …}`", OpPRReply, c.Number, c.ID)
	case KindReview:
		where = fmt.Sprintf("Review on PR #%d", c.Number)
		if c.State != "" {
			where += " (" + strings.ReplaceAll(strings.ToLower(c.State), "_", " ") + ")"
		}
		reply = fmt.Sprintf("reply with `call %s {\"number\": %d, \"body\": …}`", OpPRComment, c.Number)
	default:
		where = "Comment"
	}
	author := c.Author
	if c.Agent != nil {
		author += " (an agent)"
	}
	at := c.CreatedAt
	if at.IsZero() {
		at = c.UpdatedAt
	}
	return fmt.Sprintf("### %s by %s (%s; %s)\n%s\n\n%s", where, author, at.UTC().Format(time.RFC3339), reply, c.URL, strings.TrimSpace(c.Body))
}
