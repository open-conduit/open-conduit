package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type refusingKeyring struct{}

func (refusingKeyring) Set(string, string, string) error { return errors.New("no keyring") }
func (refusingKeyring) Remove(string, string) error      { return errors.New("no keyring") }

// The file store writes an owner-only file a file: reference resolves,
// replaces and removes names in it, and refuses bad names and values.
func TestSecretStoreFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "secrets.json")
	s := &SecretStore{File: path, Keyring: refusingKeyring{}}
	ref, fellBack, err := s.Set("api", "one", "")
	if err != nil || ref != "file:api" || !fellBack {
		t.Fatalf("fallback: %s %v %v", ref, fellBack, err)
	}
	if _, _, err = s.Set("api", "two", StoreFile); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Set("hook", "h", StoreFile); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	r := &Resolver{SecretsFile: path}
	if v, err := r.Resolve(context.Background(), ref); err != nil || v != "two" {
		t.Fatalf("resolve: %q %v", v, err)
	}
	if _, _, err = s.Set("api", "x", StoreKeyring); !errors.Is(err, ErrKeyringRefused) {
		t.Fatalf("keyring asked for: %v", err)
	}
	for _, c := range []struct{ name, value string }{{"Bad", "v"}, {"ok", ""}, {"ok", string(make([]byte, MaxSecretValue+1))}} {
		if _, _, err = s.Set(c.name, c.value, ""); err == nil {
			t.Fatalf("accepted %q", c.name)
		}
	}
	if err = s.Remove("api", StoreFile); err != nil {
		t.Fatal(err)
	}
	if err = s.Remove("api", StoreFile); err != nil {
		t.Fatalf("removing a name not held: %v", err)
	}
	if names, err := s.FileNames(); err != nil || len(names) != 1 || names[0] != "hook" {
		t.Fatalf("names: %v %v", names, err)
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Set("api", "v", StoreFile); !errors.Is(err, ErrSecretsFile) {
		t.Fatalf("a file others can read: %v", err)
	}
}
