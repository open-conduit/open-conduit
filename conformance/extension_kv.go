package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/protocol"
	"github.com/alsiola/open-conduit/storage"
)

// ExtensionKV is the namespaced KV an extension reaches through the host
// (07-02): reads with a version token and atomic precondition batches. The
// same cases run against the host's scoped service called in-process and
// against an extension's client over the protocol.
type ExtensionKV interface {
	Get(ctx context.Context, namespace, key string) ([]byte, string, error)
	List(ctx context.Context, namespace, prefix string) ([]protocol.KVEntry, error)
	Commit(ctx context.Context, batch protocol.KVCommitParams) error
}

// ExtensionKVFixture is a fresh KV for an extension called Name, plus Peek,
// a trusted read of any namespace that bypasses the extension's scoping.
type ExtensionKVFixture struct {
	KV   ExtensionKV
	Name string
	Peek func(namespace, key string) ([]byte, error)
}

type ExtensionKVCase struct {
	Name  string
	Check func(ExtensionKVFixture) error
}

func put(ns, key, value string) protocol.KVPut {
	return protocol.KVPut{Namespace: ns, Key: key, Value: []byte(value)}
}

func ExtensionKVCases() []ExtensionKVCase {
	ctx := context.Background()
	return []ExtensionKVCase{
		{"own_namespaces_and_versions", func(f ExtensionKVFixture) error {
			sub := f.Name + ".index"
			if err := f.KV.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{put(f.Name, "a", "1"), put(sub, "b", "2")}}); err != nil {
				return err
			}
			v, version, err := f.KV.Get(ctx, sub, "b")
			if err != nil || string(v) != "2" || version == "" {
				return fmt.Errorf("own sub-namespace read: %q %q %v", v, version, err)
			}
			if _, _, err := f.KV.Get(ctx, sub, "missing"); !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("absent key: %v, want ErrNotFound", err)
			}
			if err := f.KV.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{put(sub, "b", "3")}}); err != nil {
				return err
			}
			_, changed, err := f.KV.Get(ctx, sub, "b")
			if err != nil || changed == version {
				return fmt.Errorf("version did not change with the value")
			}
			return nil
		}},
		{"foreign_namespaces_refused", func(f ExtensionKVFixture) error {
			for _, ns := range []string{"other", f.Name + "x", "x" + f.Name, "core.acceptance", "core.extensions", ""} {
				if _, _, err := f.KV.Get(ctx, ns, "k"); !errors.Is(err, extension.ErrUnauthorized) {
					return fmt.Errorf("get %q: %v, want ErrUnauthorized", ns, err)
				}
				if _, err := f.KV.List(ctx, ns, ""); !errors.Is(err, extension.ErrUnauthorized) {
					return fmt.Errorf("list %q: %v, want ErrUnauthorized", ns, err)
				}
				// A batch mixing an own and a foreign write applies nothing.
				err := f.KV.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{put(f.Name, "mixed", "x"), put(ns, "k", "x")}})
				if !errors.Is(err, extension.ErrUnauthorized) {
					return fmt.Errorf("commit %q: %v, want ErrUnauthorized", ns, err)
				}
				if ns != "" {
					if _, err := f.Peek(ns, "k"); !errors.Is(err, storage.ErrNotFound) {
						return fmt.Errorf("foreign write landed in %q: %v", ns, err)
					}
				}
				if _, _, err := f.KV.Get(ctx, f.Name, "mixed"); !errors.Is(err, storage.ErrNotFound) {
					return fmt.Errorf("partial batch applied: %v", err)
				}
			}
			return nil
		}},
		{"preconditions_are_atomic", func(f ExtensionKVFixture) error {
			if err := f.KV.Commit(ctx, protocol.KVCommitParams{Checks: []protocol.KVCheck{{Namespace: f.Name, Key: "k", Absent: true}}, Puts: []protocol.KVPut{put(f.Name, "k", "1")}}); err != nil {
				return fmt.Errorf("create if absent: %v", err)
			}
			_, version, err := f.KV.Get(ctx, f.Name, "k")
			if err != nil {
				return err
			}
			stale := protocol.KVCommitParams{Checks: []protocol.KVCheck{{Namespace: f.Name, Key: "k", Absent: true}}, Puts: []protocol.KVPut{put(f.Name, "k", "2"), put(f.Name, "other", "2")}}
			if err := f.KV.Commit(ctx, stale); !errors.Is(err, storage.ErrConflict) {
				return fmt.Errorf("failed absence check: %v, want ErrConflict", err)
			}
			if _, _, err := f.KV.Get(ctx, f.Name, "other"); !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("conflicting batch partly applied")
			}
			wrong := protocol.KVCommitParams{Checks: []protocol.KVCheck{{Namespace: f.Name, Key: "k", Version: "not-the-version"}}, Deletes: []protocol.KVDelete{{Namespace: f.Name, Key: "k"}}}
			if err := f.KV.Commit(ctx, wrong); !errors.Is(err, storage.ErrConflict) {
				return fmt.Errorf("failed version check: %v, want ErrConflict", err)
			}
			right := protocol.KVCommitParams{Checks: []protocol.KVCheck{{Namespace: f.Name, Key: "k", Version: version}}, Puts: []protocol.KVPut{put(f.Name, "k", "3")}, Deletes: []protocol.KVDelete{{Namespace: f.Name, Key: "absent"}}}
			if err := f.KV.Commit(ctx, right); err != nil {
				return fmt.Errorf("matching version check (and deleting an absent key): %v", err)
			}
			if v, _, err := f.KV.Get(ctx, f.Name, "k"); err != nil || string(v) != "3" {
				return fmt.Errorf("guarded write lost: %q %v", v, err)
			}
			return nil
		}},
		{"list_is_prefix_ordered", func(f ExtensionKVFixture) error {
			ns := f.Name + ".list"
			if err := f.KV.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{put(ns, "p/b", "b"), put(ns, "p/a", "a"), put(ns, "q", "q"), put(ns, "p/c", "c")}}); err != nil {
				return err
			}
			got, err := f.KV.List(ctx, ns, "p/")
			if err != nil {
				return err
			}
			var keys []string
			for _, e := range got {
				keys = append(keys, e.Key+"="+string(e.Value))
				if e.Version == "" {
					return fmt.Errorf("list entry without version")
				}
			}
			if want := []string{"p/a=a", "p/b=b", "p/c=c"}; !reflect.DeepEqual(keys, want) {
				return fmt.Errorf("list %v, want %v", keys, want)
			}
			return nil
		}},
		{"malformed_batches_refused", func(f ExtensionKVFixture) error {
			for _, b := range []protocol.KVCommitParams{
				{},
				{Checks: []protocol.KVCheck{{Namespace: f.Name, Key: "k", Version: "v", Absent: true}}, Puts: []protocol.KVPut{put(f.Name, "k", "1")}},
				{Puts: []protocol.KVPut{{Namespace: f.Name, Key: "", Value: []byte("1")}}},
			} {
				if err := f.KV.Commit(ctx, b); err == nil {
					return fmt.Errorf("malformed batch accepted: %+v", b)
				}
			}
			return nil
		}},
	}
}

// RunExtensionKV runs the extension KV cases, one fresh fixture each.
func RunExtensionKV(t *testing.T, factory func() ExtensionKVFixture) {
	t.Helper()
	for _, c := range ExtensionKVCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(factory()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
