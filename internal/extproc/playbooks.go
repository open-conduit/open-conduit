package extproc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/open-conduit/open-conduit/protocol"
)

// PlaybookBin is the placeholder a package playbook writes where it runs
// Open Conduit's own CLI; the host replaces it with how the agent runs it.
const PlaybookBin = "{{bin}}"

// maxPlaybook caps one playbook's size.
const maxPlaybook = 256 << 10

var playbookName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Playbooks reads a package's declared playbooks directory: each
// `<name>.md` file is the playbook (a Claude Code skill) of that name, its
// text as written. Other files and directories are ignored. A package that
// declares no playbooks has none.
func Playbooks(dir string, m protocol.Manifest) (map[string]string, error) {
	if m.Provides.Playbooks == "" || dir == "" {
		return nil, nil
	}
	root := filepath.Join(dir, m.Provides.Playbooks)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("playbooks directory: %w", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || !e.Type().IsRegular() || !playbookName.MatchString(name) {
			continue
		}
		path := filepath.Join(root, e.Name())
		info, err := os.Stat(path)
		if err != nil {
			return out, err
		}
		if info.Size() > maxPlaybook {
			return out, fmt.Errorf("playbook %s is larger than %d bytes", name, maxPlaybook)
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return out, err
		}
		out[name] = string(text)
	}
	return out, nil
}

// RenderPlaybook replaces the CLI placeholder with bin.
func RenderPlaybook(text, bin string) string { return strings.ReplaceAll(text, PlaybookBin, bin) }
