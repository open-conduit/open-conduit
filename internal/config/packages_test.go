package config

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestPackagePassEnv: settings.extensions grants a package daemon
// variables by name. The grant reaches the snapshot and the normalized
// config, fails closed on the names a binding's pass_env refuses, and a
// configuration without one keeps its generation hash.
func TestPackagePassEnv(t *testing.T) {
	plain := generic(t, example(t, "git-scope"))
	before, err := Validate(encode(t, plain), registry())
	if err != nil {
		t.Fatal(err)
	}
	m := generic(t, example(t, "git-scope"))
	settings, _ := m["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
		m["settings"] = settings
	}
	settings["extensions"] = map[string]any{"ssh": map[string]any{"pass_env": []any{"SSH_AUTH_SOCK", "HTTPS_PROXY"}}, "docker": map[string]any{}}
	snap, err := Validate(encode(t, m), registry())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Settings.PackagePassEnv("ssh"); !slices.Equal(got, []string{"SSH_AUTH_SOCK", "HTTPS_PROXY"}) {
		t.Fatalf("ssh's grant %v", got)
	}
	if got := snap.Settings.PackagePassEnv("docker"); len(got) != 0 {
		t.Fatalf("docker's grant %v, want none", got)
	}
	if !strings.Contains(string(snap.Redacted()), `"extensions":{"docker":{},"ssh":{"pass_env":["SSH_AUTH_SOCK","HTTPS_PROXY"]}}`) {
		t.Fatalf("normalized config dropped the grant: %s", snap.Redacted())
	}
	again, err := Validate(snap.Normalized, registry())
	if err != nil || again.Hash != snap.Hash {
		t.Fatalf("the stored generation does not validate to itself: %v", err)
	}
	empty := generic(t, example(t, "git-scope"))
	if s, _ := empty["settings"].(map[string]any); s != nil {
		s["extensions"] = map[string]any{}
	} else {
		empty["settings"] = map[string]any{"extensions": map[string]any{}}
	}
	if snap, err := Validate(encode(t, empty), registry()); err != nil || snap.Hash != before.Hash {
		t.Fatalf("a configuration without a grant changed its hash: %v", err)
	}

	for name, tc := range map[string]struct {
		pkgs any
		refs map[string]any
		path string
		code string
	}{
		"toolbelt":     {map[string]any{"ssh": map[string]any{"pass_env": []any{"OS_TOKEN"}}}, nil, "settings/extensions/ssh/pass_env/0", "pass_env_reserved"},
		"daemon":       {map[string]any{"ssh": map[string]any{"pass_env": []any{"OPEN_CONDUIT_CONFIG"}}}, nil, "settings/extensions/ssh/pass_env/0", "pass_env_reserved"},
		"secret":       {map[string]any{"ssh": map[string]any{"pass_env": []any{"OC_TEST_GH_TOKEN"}}}, map[string]any{"token": "env:OC_TEST_GH_TOKEN"}, "settings/extensions/ssh/pass_env/0", "pass_env_secret"},
		"not a name":   {map[string]any{"ssh": map[string]any{"pass_env": []any{"SSH-AUTH"}}}, nil, "settings/extensions/ssh/pass_env/0", "invalid_value"},
		"duplicate":    {map[string]any{"ssh": map[string]any{"pass_env": []any{"A", "A"}}}, nil, "settings/extensions/ssh/pass_env/1", "duplicate_value"},
		"unknown key":  {map[string]any{"ssh": map[string]any{"env": []any{"A"}}}, nil, "settings/extensions/ssh/env", "unknown_field"},
		"package name": {map[string]any{"SSH": map[string]any{}}, nil, "settings/extensions/SSH", "invalid_value"},
		"not object":   {[]any{"ssh"}, nil, "settings/extensions", "invalid_type"},
	} {
		m := generic(t, example(t, "git-scope"))
		settings, _ := m["settings"].(map[string]any)
		if settings == nil {
			settings = map[string]any{}
			m["settings"] = settings
		}
		settings["extensions"] = tc.pkgs
		if tc.refs != nil {
			m["scopes"].([]any)[0].(map[string]any)["secret_refs"] = tc.refs
		}
		_, err := Validate(encode(t, m), registry())
		found := false
		for _, d := range diagnostics(t, err) {
			found = found || d.Code == tc.code && d.Path == tc.path
		}
		if !found {
			t.Errorf("%s: diagnostics %+v, want %s at %s", name, diagnostics(t, err), tc.code, tc.path)
		}
	}
}

// TestPackageWarnings: a grant to a package that isn't installed or
// bundled is a warning, not an error, so a config survives removing one.
func TestPackageWarnings(t *testing.T) {
	m := generic(t, example(t, "git-scope"))
	settings, _ := m["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
		m["settings"] = settings
	}
	settings["extensions"] = map[string]any{"ssh": map[string]any{"pass_env": []any{"SSH_AUTH_SOCK"}}, "gone": map[string]any{"pass_env": []any{"X"}}}
	reg := registry()
	reg.Packages = map[string]bool{"ssh": true}
	snap, err := Validate(encode(t, m), reg)
	if err != nil {
		t.Fatalf("a grant to a missing package refused: %v", err)
	}
	got := PackageWarnings(snap, reg)
	if len(got) != 1 || !strings.Contains(got[0], "settings.extensions.gone") {
		t.Fatalf("warnings %v, want one for gone", got)
	}
	// Negative control: with both installed, nothing is warned about.
	reg.Packages["gone"] = true
	if got := PackageWarnings(snap, reg); len(got) != 0 {
		t.Fatalf("warnings %v with every package installed", got)
	}
}

// TestPackageGrants: a document read before validation (the config file
// the daemon starts its packages from) never grants what validation would
// refuse.
func TestPackageGrants(t *testing.T) {
	data := []byte(`{"version":1,"settings":{"extensions":{"ssh":{"pass_env":["SSH_AUTH_SOCK","OS_TOKEN","GH_TOKEN"]},"docker":{}}},"connections":[],
		"scopes":[{"id":"a","name":"A","authorization":{"connections":[],"operations":[],"read_scopes":[]},"secret_refs":{"gh":"env:GH_TOKEN"}}]}`)
	got := FileGrants(data)
	want := map[string][]string{"ssh": {"SSH_AUTH_SOCK"}}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("grants %v, want %v", got, want)
	}
	if got := FileGrants([]byte(`{"version":1,"settings":{"extensions":{"ssh":{"pass_env":["SSH_AUTH_SOCK"]}}},"bogus":1}`)); len(got) != 0 {
		t.Fatalf("a file that doesn't parse granted %v", got)
	}
}

// TestExtensionTrust: settings.extension_trust reaches the snapshot and
// survives normalization (a stored generation must start packages under
// the same policy), an empty one is still a policy, a configuration without
// one keeps its generation hash, and each malformed entry is refused.
func TestExtensionTrust(t *testing.T) {
	withTrust := func(trust any) map[string]any {
		m := generic(t, example(t, "git-scope"))
		settings, _ := m["settings"].(map[string]any)
		if settings == nil {
			settings = map[string]any{}
			m["settings"] = settings
		}
		settings["extension_trust"] = trust
		return m
	}
	digest := "sha256:" + strings.Repeat("ab", 32)
	const issuer = "https://token.actions.githubusercontent.com"
	snap, err := Validate(encode(t, withTrust(map[string]any{
		"publishers": []any{
			map[string]any{"subject_regexp": `https://github\.com/acme/ext/.+`, "issuer": issuer, "packages": []any{"acme"}},
			map[string]any{"subject": "https://github.com/beta/ext/.github/workflows/release.yml@refs/heads/main", "issuer": issuer},
		},
		"pinned": map[string]any{"local": []any{digest}},
	})), registry())
	if err != nil {
		t.Fatal(err)
	}
	want := &ExtensionTrust{
		Publishers: []TrustedPublisher{
			{SubjectRegexp: `https://github\.com/acme/ext/.+`, Issuer: issuer, Packages: []string{"acme"}},
			{Subject: "https://github.com/beta/ext/.github/workflows/release.yml@refs/heads/main", Issuer: issuer},
		},
		Pinned: map[string][]string{"local": {strings.Repeat("ab", 32)}},
	}
	if !reflect.DeepEqual(snap.Settings.ExtensionTrust, want) {
		t.Fatalf("policy %+v", snap.Settings.ExtensionTrust)
	}
	again, err := Validate(snap.Normalized, registry())
	if err != nil || again.Hash != snap.Hash || !reflect.DeepEqual(again.Settings.ExtensionTrust, want) {
		t.Fatalf("the stored generation lost the policy: %+v %v", again, err)
	}
	doc, err := Parse(snap.Normalized)
	if err != nil || !reflect.DeepEqual(doc.Settings.ExtensionTrust, want) {
		t.Fatalf("the parsed stored generation lost the policy: %v", err)
	}
	if got, err := ExtensionTrustOf(snap.Normalized); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("read alone: %+v %v", got, err)
	}

	empty, err := Validate(encode(t, withTrust(map[string]any{})), registry())
	if err != nil || empty.Settings.ExtensionTrust == nil {
		t.Fatalf("an empty policy is still a policy: %+v %v", empty, err)
	}
	plain, err := Validate(encode(t, generic(t, example(t, "git-scope"))), registry())
	if err != nil || plain.Settings.ExtensionTrust != nil {
		t.Fatalf("no policy: %v", err)
	}
	if empty.Hash == plain.Hash {
		t.Fatal("adding a policy kept the generation hash")
	}

	for name, tc := range map[string]struct {
		trust      any
		path, code string
	}{
		"not an object":   {[]any{}, "settings/extension_trust", "invalid_type"},
		"unknown field":   {map[string]any{"allow": true}, "settings/extension_trust/allow", "unknown_field"},
		"no issuer":       {map[string]any{"publishers": []any{map[string]any{"subject": "x"}}}, "settings/extension_trust/publishers/0/issuer", "missing_field"},
		"no subject":      {map[string]any{"publishers": []any{map[string]any{"issuer": issuer}}}, "settings/extension_trust/publishers/0", "invalid_value"},
		"both subjects":   {map[string]any{"publishers": []any{map[string]any{"issuer": issuer, "subject": "x", "subject_regexp": "x"}}}, "settings/extension_trust/publishers/0", "invalid_value"},
		"bad regexp":      {map[string]any{"publishers": []any{map[string]any{"issuer": issuer, "subject_regexp": "("}}}, "settings/extension_trust/publishers/0/subject_regexp", "invalid_value"},
		"empty subject":   {map[string]any{"publishers": []any{map[string]any{"issuer": issuer, "subject": ""}}}, "settings/extension_trust/publishers/0/subject", "invalid_value"},
		"bad package":     {map[string]any{"publishers": []any{map[string]any{"issuer": issuer, "subject": "x", "packages": []any{"Acme"}}}}, "settings/extension_trust/publishers/0/packages/0", "invalid_value"},
		"bare hex pin":    {map[string]any{"pinned": map[string]any{"acme": []any{strings.Repeat("ab", 32)}}}, "settings/extension_trust/pinned/acme/0", "invalid_value"},
		"short pin":       {map[string]any{"pinned": map[string]any{"acme": []any{"sha256:abcd"}}}, "settings/extension_trust/pinned/acme/0", "invalid_value"},
		"bad pinned name": {map[string]any{"pinned": map[string]any{"Acme": []any{digest}}}, "settings/extension_trust/pinned/Acme", "invalid_value"},
		"duplicate pin":   {map[string]any{"pinned": map[string]any{"acme": []any{digest, digest}}}, "settings/extension_trust/pinned/acme/1", "duplicate_value"},
	} {
		_, err := Validate(encode(t, withTrust(tc.trust)), registry())
		found := false
		for _, d := range diagnostics(t, err) {
			found = found || d.Code == tc.code && d.Path == tc.path
		}
		if !found {
			t.Errorf("%s: diagnostics %+v, want %s at %s", name, diagnostics(t, err), tc.code, tc.path)
		}
	}
}
