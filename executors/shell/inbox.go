package shell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// The inbox is the shell executor's inject channel (04-02, the generic inject
// acknowledgement). A segment's run directory holds inbox/, named to the
// process as OS_INBOX. Inject writes each accepted Event there as
// <order>-<id digest>.json (the OS_INPUT_FILE event shape). The process takes
// input in by reading the inbox and acknowledging each event it read with
// inbox/consumed/<id digest> (the `os inbox` command does both). The result
// reports acknowledged events, and only those core put in the inbox, as
// Result.ConsumedInputs. The inbox lives on disk, so an adopted process keeps
// receiving input; once the result is recorded nothing more is accepted.

const inboxDir = "inbox"

func eventDigest(id domain.ID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:12])
}

// Inject delivers an accepted Event to the running segment's inbox. Success
// means the file is durably in the inbox, not that the process read it.
func (e *Executor) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ev.ID == "" {
		return extension.ErrInvalid
	}
	e.mu.Lock()
	rn := e.runs[id]
	e.mu.Unlock()
	if rn == nil {
		return extension.ErrNotAccepting
	}
	// The finalize lock orders every delivery before or after the result.
	unlock, err := lockFile(filepath.Join(rn.dir, "finalize.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if _, ok, err := readResult(rn.dir); err != nil {
		return err
	} else if ok {
		return extension.ErrNotAccepting
	}
	dir := filepath.Join(rn.dir, inboxDir)
	if err := os.MkdirAll(filepath.Join(dir, "consumed"), 0o700); err != nil {
		return err
	}
	digest := eventDigest(ev.ID)
	entries, err := inboxEntries(dir)
	if err != nil {
		return err
	}
	for _, name := range entries {
		if strings.HasSuffix(strings.TrimSuffix(name, ".json"), "-"+digest) {
			return nil // a retried dispatch of the same Event
		}
	}
	return writeJSON(filepath.Join(dir, fmt.Sprintf("%06d-%s.json", len(entries)+1, digest)), toInput(ev))
}

// inboxEntries lists delivered event files in delivery order.
func inboxEntries(dir string) ([]string, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range files {
		if f.Type().IsRegular() && strings.HasSuffix(f.Name(), ".json") && !strings.HasPrefix(f.Name(), ".") {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// consumedInputs is the proof for a finishing segment: the delivered events
// the process acknowledged, in delivery order. An acknowledgement for anything
// not delivered is ignored.
func consumedInputs(runDir string) []domain.ID {
	dir := filepath.Join(runDir, inboxDir)
	names, err := inboxEntries(dir)
	if err != nil {
		return nil
	}
	var out []domain.ID
	for _, name := range names {
		digest := name[strings.LastIndex(name, "-")+1 : len(name)-len(".json")]
		if _, err := os.Stat(filepath.Join(dir, "consumed", digest)); err != nil {
			continue
		}
		var ev inputEvent
		if readJSON(filepath.Join(dir, name), &ev) != nil || ev.ID == "" || eventDigest(domain.ID(ev.ID)) != digest {
			continue
		}
		out = append(out, domain.ID(ev.ID))
	}
	return out
}
