package config

import (
	"strings"
	"testing"
)

// Diagnostics name the remedy: the nearest field or registration for a
// typo, what is registered, and where invalid JSON breaks.
func TestDiagnosticRemedies(t *testing.T) {
	base := string(example(t, "shell-job"))
	cases := map[string]struct {
		doc, path, code, want, notWant string
	}{
		"field typo": {strings.Replace(base, `"settings": {}`, `"settings": {"concurency": 2}`, 1), "settings/concurency", "unknown_field", `did you mean "concurrency"?`, ""},
		// A name close to nothing gets the allowed list, not a guess.
		"unrelated field": {strings.Replace(base, `"name": "maintenance",`, `"name": "maintenance", "zzzzzz": 1,`, 1), "scopes/0/zzzzzz", "unknown_field", "allowed: ", "did you mean"},
		"executor typo":   {strings.Replace(base, `"executor": "shell"`, `"executor": "shel"`, 1), "scopes/0/bindings/0/executor", "unknown_registration", `did you mean "shell"?`, ""},
		"executor list":   {strings.Replace(base, `"executor": "shell"`, `"executor": "robot"`, 1), "scopes/0/bindings/0/executor", "unknown_registration", "registered: coding_agent, shell, support_agent", "did you mean"},
		"extension list":  {strings.Replace(base, `"type": "schedule"`, `"type": "cron"`, 1), "scopes/0/extensions/0/type", "unknown_registration", "registered: chat_ingress, code_ingress, schedule", ""},
		"json position":   {"{\n  \"version\": 1,\n  \"settings\": {,\n}", "", "invalid_json", "line 3, column 16", ""},
		"json truncated":  {"{\n  \"version\": 1,", "", "invalid_json", "ends early", ""},
	}
	for name, c := range cases {
		_, err := Validate([]byte(c.doc), registry())
		var got *Diagnostic
		for _, d := range diagnostics(t, err) {
			if d.Path == c.path && d.Code == c.code {
				got = &d
			}
		}
		if got == nil {
			t.Errorf("%s: no %s at %q: %v", name, c.code, c.path, err)
			continue
		}
		if !strings.Contains(got.Message, c.want) || c.notWant != "" && strings.Contains(got.Message, c.notWant) {
			t.Errorf("%s: message %q, want %q without %q", name, got.Message, c.want, c.notWant)
		}
	}
}

// A binding, route or extension may leave out its empty collections;
// they default to empty rather than each being a missing field.
func TestEmptyCollectionsDefault(t *testing.T) {
	base := string(example(t, "shell-job"))
	short := strings.NewReplacer(
		`"secret_names": [],`, ``,
		`"active_policy": "queue",
          "config": {}`, `"active_policy": "queue"`,
	).Replace(base)
	short = strings.Replace(short, `"requires": {
            "toolbelt": "cli"
          },`, ``, 1)
	if short == base {
		t.Fatal("fixture replace failed")
	}
	full, err := Validate([]byte(base), registry())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Validate([]byte(short), registry())
	if err != nil {
		t.Fatalf("defaulted collections refused: %v", err)
	}
	if snap.Hash == full.Hash {
		t.Fatal("requires removal should change the document")
	}
	// The required names still are.
	if _, err = Validate([]byte(strings.Replace(base, `"executor": "shell",`, ``, 1)), registry()); err == nil || !strings.Contains(err.Error(), "missing_field") {
		t.Fatalf("binding without executor: %v", err)
	}
}

func TestNearest(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{"concurency", "concurrency"}, {"admision_limit", "admission_limit"}, {"cnocurrency", "concurrency"}, {"x", ""}, {"zzzzzz", ""},
	} {
		if n := nearest(c.got, []string{"concurrency", "admission_limit", "webhooks"}); n != c.want {
			t.Errorf("nearest(%q) = %q, want %q", c.got, n, c.want)
		}
	}
}
