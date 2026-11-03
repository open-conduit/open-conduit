package git

import "github.com/open-conduit/open-conduit/protocol"

// ResourceFields describe a git resource config (Config) for the
// operator's catalogue (manifest fields).
func ResourceFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "checkout", Help: "the main checkout", Required: true},
		{Name: "mode", Help: "the mode when an executor requests none", Default: "shared", Options: []string{"shared", "work", "fresh", "clone"}},
		{Name: "remote", Default: "origin"},
		{Name: "base_branch", Help: "where new branches and fresh checkouts start; the remote's HEAD by default"},
		{Name: "branch_template", Help: "a work-mode branch, with {work} and {scope}", Default: "agent/work-{work}"},
		{Name: "sign", Help: "keep commit signing as the checkout configures it", Default: "false"},
		{Name: "copy", Help: "checkout-relative files (such as .env) copied into every workspace"},
		{Name: "setup", Help: "a command run in a workspace when it or a setup_when file changed"},
		{Name: "setup_when"},
		{Name: "setup_timeout_seconds", Default: "1800"},
		{Name: "dotenv", Help: "the checkout's .env as the provider environment layer"},
		{Name: "retain_failed_seconds", Help: "how long a failed run's workspace is kept for inspection", Default: "86400"},
		{Name: "disk_full_percent", Help: "a new workspace may not leave the disk fuller; 100 turns the check off", Default: "90"},
		{Name: "disk_reserve_bytes", Help: "the most kept free, default 10 GiB"},
	}
}
