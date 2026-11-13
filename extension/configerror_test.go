package extension_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

type decoded struct {
	Name  string            `json:"name"`
	Every int               `json:"every_minutes,omitempty"`
	Jobs  []decodedJob      `json:"jobs"`
	Env   map[string]string `json:"env,omitempty"`
	On    *struct {
		Webhook bool `json:"webhook"`
	} `json:"on,omitempty"`
}

type decodedJob struct {
	Cron string `json:"cron"`
}

// TestDecodeConfig: a strict decode names each unknown field and each
// value of the wrong type at its JSON pointer, and accepts a good config.
func TestDecodeConfig(t *testing.T) {
	var ok decoded
	if err := extension.DecodeConfig([]byte(`{"Name":"a","jobs":[{"cron":"* * * * *"}],"env":{"A":"b"},"on":{"webhook":true}}`), &ok); err != nil || ok.Name != "a" || len(ok.Jobs) != 1 {
		t.Fatalf("%+v %v", ok, err)
	}
	if err := extension.DecodeConfig(nil, &ok); err != nil {
		t.Fatalf("empty config: %v", err)
	}
	for _, c := range []struct{ in, path, code string }{
		{`{"jobs":[{"cron":"x"},{"crom":"x"}]}`, "/jobs/1/crom", "unknown_field"},
		{`{"every_minutes":"5"}`, "/every_minutes", "invalid_type"},
		{`{"every_minutes":1.5}`, "/every_minutes", "invalid_type"},
		{`{"env":{"a/b":1}}`, "/env/a~1b", "invalid_type"},
		{`{"on":{"webhook":"yes"}}`, "/on/webhook", "invalid_type"},
		{`{"jobs":{}}`, "/jobs", "invalid_type"},
		{`[]`, "", "invalid_type"},
		{`{`, "", "malformed_config"},
		{`{} {}`, "", "malformed_config"},
	} {
		err := extension.DecodeConfig([]byte(c.in), &decoded{})
		ds := extension.ConfigDiagnostics(err)
		if !errors.Is(err, extension.ErrInvalid) || len(ds) != 1 || ds[0].Path != c.path || ds[0].Code != c.code {
			t.Errorf("%s: %+v (%v); want %s %s", c.in, ds, err, c.path, c.code)
		}
	}
	ds := extension.ConfigDiagnostics(extension.DecodeConfig([]byte(`{"nme":"a"}`), &decoded{}))
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "name, every_minutes, jobs, env, on") {
		t.Errorf("unknown field doesn't list the allowed ones: %+v", ds)
	}
}

// TestConfigDiagnosticsLimits: what the host passes through is bounded; a
// diagnostic outside the limits is dropped, the rest kept, and an error
// that is not a ConfigError carries none (negative control).
func TestConfigDiagnosticsLimits(t *testing.T) {
	if ds := extension.ConfigDiagnostics(errors.New("cron \"secret\" is bad")); ds != nil {
		t.Fatalf("a plain error passed through: %+v", ds)
	}
	ce := &extension.ConfigError{}
	ce.Add("/ok", "fine", "kept")
	ce.Add("no-slash", "fine", "dropped: path")
	ce.Add("/x~2", "fine", "dropped: escape")
	ce.Add("/x", "Bad-Code", "dropped: code")
	ce.Add("/x", "fine", "")
	ce.Add("/x", "fine", "line\nbreak")
	ce.Add("/x", "fine", strings.Repeat("a", extension.MaxConfigMessage+1))
	for range extension.MaxConfigDiagnostics + 5 {
		ce.Add("", "more", "kept up to the limit")
	}
	ds := extension.ConfigDiagnostics(errors.Join(errors.New("wrapped"), ce))
	if len(ds) != extension.MaxConfigDiagnostics || ds[0].Message != "kept" || ds[1].Code != "more" {
		t.Fatalf("%d %+v", len(ds), ds[:2])
	}
	if p := extension.ConfigPointer("jobs", 1, "a/b~c"); p != "/jobs/1/a~1b~0c" {
		t.Fatalf("pointer %s", p)
	}
}
