package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type refusingKeyring struct{}

func (refusingKeyring) Set(context.Context, string, string, string) error {
	return errors.New("no keyring")
}
func (refusingKeyring) Remove(context.Context, string, string) error { return errors.New("no keyring") }

// The file store writes an owner-only file a file: reference resolves,
// replaces and removes names in it, and refuses bad names and values.
func TestSecretStoreFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cfg", "secrets.json")
	s := &SecretStore{File: path, Keyring: refusingKeyring{}}
	ref, fellBack, err := s.Set(ctx, "api", "one", "")
	if err != nil || ref != "file:api" || !fellBack {
		t.Fatalf("fallback: %s %v %v", ref, fellBack, err)
	}
	if _, _, err = s.Set(ctx, "api", "two", StoreFile); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Set(ctx, "hook", "h", StoreFile); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	r := &Resolver{SecretsFile: path}
	if v, err := r.Resolve(ctx, ref); err != nil || v != "two" {
		t.Fatalf("resolve: %q %v", v, err)
	}
	if _, _, err = s.Set(ctx, "fresh", "x", StoreKeyring); !errors.Is(err, ErrKeyringRefused) {
		t.Fatalf("keyring asked for: %v", err)
	}
	for _, c := range []struct{ name, value string }{{"Bad", "v"}, {"ok", ""}, {"ok", string(make([]byte, MaxSecretValue+1))}} {
		if _, _, err = s.Set(ctx, c.name, c.value, ""); err == nil {
			t.Fatalf("accepted %q", c.name)
		}
	}
	if err = s.Remove(ctx, "api", StoreFile); err != nil {
		t.Fatal(err)
	}
	if err = s.Remove(ctx, "api", StoreFile); err != nil {
		t.Fatalf("removing a name not held: %v", err)
	}
	if names, err := s.FileNames(); err != nil || len(names) != 1 || names[0] != "hook" {
		t.Fatalf("names: %v %v", names, err)
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Set(ctx, "api", "v", StoreFile); !errors.Is(err, ErrSecretsFile) {
		t.Fatalf("a file others can read: %v", err)
	}
}

// recordingKeyring is a keyring that stores values and reads them back.
type recordingKeyring map[string]string

func (k recordingKeyring) Set(_ context.Context, service, name, value string) error {
	k[service+"/"+name] = value
	return nil
}
func (k recordingKeyring) Remove(_ context.Context, service, name string) error {
	delete(k, service+"/"+name)
	return nil
}
func (k recordingKeyring) Get(service, name string) (string, error) {
	if v, ok := k[service+"/"+name]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}

// A stored: reference reads whichever store holds the name, and the store
// keeps the copy it reads current, so a stale keyring value is never served.
func TestStoredReference(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secrets.json")
	k := recordingKeyring{}
	s := &SecretStore{File: path, Keyring: k}
	r := &Resolver{SecretsFile: path, Keyring: k}
	const ref = "stored:api"
	if !ValidSecretRef(ref) || ProviderScheme(ref) != "" || ValidSecretRef("stored:Bad Name") {
		t.Fatal("stored: is a built-in form taking a secret name")
	}
	if _, err := r.Resolve(ctx, ref); SecretCode(err) != "stored_missing" {
		t.Fatalf("nothing stored: %v", err)
	}
	if got, _, err := s.Set(ctx, "api", "in-keyring", ""); err != nil || got != "keyring:api" {
		t.Fatalf("keyring: %s %v", got, err)
	}
	if v, err := r.Resolve(ctx, ref); err != nil || v != "in-keyring" {
		t.Fatalf("from the keyring: %q %v", v, err)
	}
	// The value moves to the file, as it does when the keyring goes away.
	// The keyring's copy is now stale and must not be served.
	if _, _, err := s.Set(ctx, "api", "in-file", StoreFile); err != nil {
		t.Fatal(err)
	}
	if v, err := r.Resolve(ctx, ref); err != nil || v != "in-file" {
		t.Fatalf("the file wins: %q %v", v, err)
	}
	// A later default write replaces the copy that is read.
	if got, fellBack, err := s.Set(ctx, "api", "newer", ""); err != nil || got != "file:api" || fellBack {
		t.Fatalf("default write of a name in the file: %s %v %v", got, fellBack, err)
	}
	if v, _ := r.Resolve(ctx, ref); v != "newer" {
		t.Fatalf("after the default write: %q", v)
	}
	if k["open-conduit/api"] != "in-keyring" {
		t.Fatalf("the keyring was written: %q", k["open-conduit/api"])
	}
	// Writing the keyring alone would leave stored: reading the old value.
	if _, _, err := s.Set(ctx, "api", "shadowed", StoreKeyring); !errors.Is(err, ErrSecretInFile) {
		t.Fatalf("keyring write shadowed by the file: %v", err)
	}
	// A file that can't be read fails closed, rather than serving the
	// keyring's copy of a value the file may have replaced.
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := r.Resolve(ctx, ref); err == nil || SecretCode(err) != "file_invalid" {
		t.Fatalf("unreadable file: %q %v", v, err)
	}
}
