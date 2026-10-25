package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
)

// RenderEvent renders one GitHub Event the way an agent reads it at the
// start of a turn (Open Supervisor's core.Render), with each comment's
// reply hint naming the tool that answers it. ok is false for Events this
// extension does not own; the host falls back to the generic renderer.
func RenderEvent(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeCommentHuman, TypeCommentAgent, TypeCommentBot:
		var c CommentPayload
		if e.Data.Schema != SchemaComment || json.Unmarshal(e.Data.Data, &c) != nil {
			return "", false
		}
		return renderComment(c), true
	case TypeChecksPassed, TypeChecksFailed, TypeChecksNone:
		var c ChecksPayload
		if e.Data.Schema != SchemaChecks || json.Unmarshal(e.Data.Data, &c) != nil {
			return "", false
		}
		switch e.Type {
		case TypeChecksNone:
			return fmt.Sprintf("### No checks ran on PR #%d at %s\nThe repo has no CI or deploy previews: treat the PR as checked once what the project's rules say must pass has passed locally.", c.Number, short(c.SHA)), true
		case TypeChecksPassed:
			s := fmt.Sprintf("### Checks passed on PR #%d at %s", c.Number, short(c.SHA))
			if c.Preview != "" {
				s += "\nPreview: " + c.Preview
			}
			return s, true
		}
		return fmt.Sprintf("### Checks failed on PR #%d at %s: %s\nSee `call %s {\"number\": %d}` for links to each.", c.Number, short(c.SHA), strings.Join(c.Failed, ", "), OpChecks, c.Number), true
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
	case TypeDependencyMet:
		var p struct {
			SourceType string `json:"source_type"`
			Label      string `json:"label"`
		}
		_ = json.Unmarshal(e.Data.Data, &p)
		c, err := ParseCond(p.Label)
		if err != nil {
			return "### What you were waiting for\nWhat you were waiting for has happened.", true
		}
		return fmt.Sprintf("### What you were waiting for\nYou were waiting for %s. That has happened: %s. If you were also waiting for something else that hasn't happened, wait again if you still need it.", c.Describe(), c.Happened(p.SourceType)), true
	case "core.wait.timeout":
		return "### What you were waiting for\nIt is now past the latest time you said to wait until, and none of what you waited for has happened. Check where it has got to, and wait again if it still matters.", true
	case TypeHoldReleased:
		var p ReleasedPayload
		_ = json.Unmarshal(e.Data.Data, &p)
		return "### What you were waiting for\n" + strings.TrimSpace(p.Why), true
	}
	return "", false
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
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
