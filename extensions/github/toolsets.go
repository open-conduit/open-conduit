package github

import (
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
)

// The GitHub toolsets: what an agent binding is granted for each role it
// plays, by name ("@github.author"), so a release that gives a role a new
// operation gives it to every install granting the toolset. Add an
// operation with the next Since; never remove one.
const (
	// ToolsetAuthor works an issue: reads it, comments, opens and edits
	// its PRs, answers their reviews and waits on them.
	ToolsetAuthor = Type + ".author"
	// ToolsetReviewer reviews PRs: reads them and their checks, comments,
	// replies in and resolves threads, and submits reviews.
	ToolsetReviewer = Type + ".reviewer"
	// ToolsetWorkspace is what a binding with a git workspace needs: git
	// asks for its push credential through the helper.
	ToolsetWorkspace = Type + ".workspace"
	// ToolsetHold is the github-hold binding's: it reads and settles holds.
	ToolsetHold = Type + ".holds"
)

func since(v int, names ...string) []extension.ToolsetOperation {
	out := make([]extension.ToolsetOperation, len(names))
	for i, n := range names {
		out[i] = extension.ToolsetOperation{Name: n, Since: v}
	}
	return out
}

// Toolsets are the GitHub extension's published toolsets. The author
// toolset's versions follow the operations' releases: github.pr.review
// joined in 2, github.pr.resolve in 3 and github.pr.edit in 4.
func Toolsets() []extension.Toolset {
	author := since(1, extension.ReadContext, extension.Wait, extension.DeclareOutcome,
		"github.context", "github.issue.view", "github.issue.comment", "github.issue.list", "github.issue.create",
		"github.pr.list", "github.pr.view", "github.pr.create", "github.pr.comment", "github.pr.reply", "github.checks",
		"github.attachment.upload", "github.attachment.get", "github.wait")
	author = append(author, since(2, "github.pr.review")...)
	author = append(author, since(3, "github.pr.resolve")...)
	author = append(author, since(4, "github.pr.edit")...)
	return []extension.Toolset{
		{Name: ToolsetAuthor, Summary: "Work a GitHub issue: read it, comment, open and edit its PRs, answer their reviews and wait on them.", Operations: author},
		{Name: ToolsetReviewer, Summary: "Review GitHub PRs: read them and their checks, comment, reply in and resolve threads, and submit reviews.",
			Operations: since(1, extension.ReadContext, extension.Wait, extension.DeclareOutcome,
				"github.context", "github.issue.view", "github.pr.list", "github.pr.view", "github.pr.comment", "github.pr.reply",
				"github.pr.resolve", "github.pr.review", "github.checks", "github.attachment.upload", "github.attachment.get", "github.wait")},
		{Name: ToolsetWorkspace, Summary: "Push from a git workspace: git's credential helper asks for a short-lived token.", Operations: since(1, OpGitCredential)},
		{Name: ToolsetHold, Summary: "Run held issues: read and settle holds.", Operations: since(1, extension.Emit, "github.hold.status", "github.hold")},
	}
}

// Toolsets are the extension's published toolsets (inproc.Toolsetter).
func (*Extension) Toolsets() []extension.Toolset { return Toolsets() }

var _ inproc.Toolsetter = (*Extension)(nil)
