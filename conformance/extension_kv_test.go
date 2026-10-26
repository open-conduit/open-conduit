package conformance_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// memoryKV is a minimal extension KV for the suite's own controls; scoped
// false models a host that forgets namespace scoping, atomic false one that
// applies a batch before checking it.
type memoryKV struct {
	mu             sync.Mutex
	name           string
	data           map[string][]byte
	scoped, atomic bool
}

func digest(v []byte) string { s := sha256.Sum256(v); return hex.EncodeToString(s[:8]) }

func (m *memoryKV) own(ns, key string) error {
	if m.scoped && (key == "" || !protocol.Namespace(m.name, ns)) || ns == "" || key == "" {
		return extension.ErrUnauthorized
	}
	return nil
}
func (m *memoryKV) Get(_ context.Context, ns, key string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.own(ns, key); err != nil {
		return nil, "", err
	}
	v, ok := m.data[ns+"\x00"+key]
	if !ok {
		return nil, "", storage.ErrNotFound
	}
	return v, digest(v), nil
}
func (m *memoryKV) List(_ context.Context, ns, prefix string) ([]protocol.KVEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.own(ns, "-"); err != nil {
		return nil, err
	}
	var out []protocol.KVEntry
	for k, v := range m.data {
		if n, key, _ := strings.Cut(k, "\x00"); n == ns && strings.HasPrefix(key, prefix) {
			out = append(out, protocol.KVEntry{Key: key, Value: v, Version: digest(v)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (m *memoryKV) Commit(_ context.Context, b protocol.KVCommitParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(b.Puts)+len(b.Deletes) == 0 {
		return extension.ErrInvalid
	}
	apply := func() error {
		for _, p := range b.Puts {
			if err := m.own(p.Namespace, p.Key); err != nil {
				return err
			}
			m.data[p.Namespace+"\x00"+p.Key] = p.Value
		}
		for _, d := range b.Deletes {
			delete(m.data, d.Namespace+"\x00"+d.Key)
		}
		return nil
	}
	if !m.atomic {
		if err := apply(); err != nil {
			return err
		}
	}
	for _, c := range b.Checks {
		if c.Absent == (c.Version != "") {
			return extension.ErrInvalid
		}
		v, ok := m.data[c.Namespace+"\x00"+c.Key]
		if c.Absent && ok || !c.Absent && (!ok || digest(v) != c.Version) {
			return storage.ErrConflict
		}
	}
	for _, p := range b.Puts {
		if err := m.own(p.Namespace, p.Key); err != nil {
			return err
		}
	}
	if m.atomic {
		return apply()
	}
	return nil
}

func kvFixture(scoped, atomic bool) conformance.ExtensionKVFixture {
	m := &memoryKV{name: "fixture", data: map[string][]byte{}, scoped: scoped, atomic: atomic}
	return conformance.ExtensionKVFixture{KV: m, Name: "fixture", Peek: func(ns, key string) ([]byte, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		v, ok := m.data[ns+"\x00"+key]
		if !ok {
			return nil, storage.ErrNotFound
		}
		return v, nil
	}}
}

func TestExtensionKVReference(t *testing.T) {
	conformance.RunExtensionKV(t, func() conformance.ExtensionKVFixture { return kvFixture(true, true) })
}

func TestWeakenedExtensionKVIsRejected(t *testing.T) {
	for name, f := range map[string]func() conformance.ExtensionKVFixture{
		"unscoped":   func() conformance.ExtensionKVFixture { return kvFixture(false, true) },
		"non_atomic": func() conformance.ExtensionKVFixture { return kvFixture(true, false) },
	} {
		failed := false
		for _, c := range conformance.ExtensionKVCases() {
			if c.Check(f()) != nil {
				failed = true
			}
		}
		if !failed {
			t.Fatalf("%s KV passed the extension KV suite", name)
		}
	}
}
