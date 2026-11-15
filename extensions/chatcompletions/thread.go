package chatcompletions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// thread is a Work's conversation, the executor's private continuity. Core
// never reads it. It is saved after every message, so an attempt that ends
// partway (a rate limit, a restart) leaves a later one the conversation to
// carry on from.
type thread struct {
	// Messages are the conversation without its system message, which
	// each attempt writes afresh from the toolbelt's current guidance.
	Messages []Message `json:"messages"`
	// Open is true while a turn has begun and not ended (with a wait, a
	// declared outcome or a final reply). A later attempt continues an
	// open turn rather than starting another.
	Open bool `json:"open,omitempty"`
	// Presented are the inputs the open turn has been shown, so a later
	// attempt shows only those it hasn't.
	Presented []domain.ID `json:"presented,omitempty"`
	// Nudged is true once the open turn was reminded to end itself.
	Nudged bool `json:"nudged,omitempty"`
	// Note is the last ended turn's summary, which a fresh conversation
	// starts from.
	Note string `json:"note,omitempty"`
	// PromptTokens is what the last request's prompt carried.
	PromptTokens uint64 `json:"prompt_tokens,omitempty"`
	// Oversize is set when the endpoint refused the conversation as
	// longer than the model's context.
	Oversize bool `json:"oversize,omitempty"`
	// Turns counts the turns begun on this conversation.
	Turns int       `json:"turns,omitempty"`
	At    time.Time `json:"at,omitzero"`
}

func key(id domain.ID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

func (e *Executor) threadPath(work domain.ID) string {
	return filepath.Join(e.o.StateDir, "work", key(work)+".json")
}

// loadThread reads a Work's conversation; a missing one is empty.
func (e *Executor) loadThread(work domain.ID) (thread, error) {
	var t thread
	data, err := os.ReadFile(e.threadPath(work))
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	} else if err != nil {
		return t, err
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return thread{}, err
	}
	return t, nil
}

// saveThread replaces a Work's conversation atomically.
func (e *Executor) saveThread(work domain.ID, t thread) error {
	return writeAtomic(e.threadPath(work), t)
}

func writeAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
