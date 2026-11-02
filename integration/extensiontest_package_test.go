package integration_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extensiontest"
)

// TestExtensiontestPackage tests a package the way its own repository
// would: no hand-written config, a scripted participant on the shell
// executor and a test secret pointing the package at a fake of its
// service. The participant asks for a forecast through the `os` CLI,
// reports it and completes; a call the package refuses comes back to it as
// a refusal.
func TestExtensiontestPackage(t *testing.T) {
	testWeatherPackage(t, "completed")
}

// TestExtensiontestPackageFakeClaude is the same test with the opt-in
// FakeClaude agent on a claude-code binding, its calls through `os mcp`.
func TestExtensiontestPackageFakeClaude(t *testing.T) {
	testWeatherPackage(t, "finished", extensiontest.UseFakeClaude())
}

func testWeatherPackage(t *testing.T, end string, options ...extensiontest.Option) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		w.Write([]byte("rain in " + r.URL.Query().Get("city")))
	}))
	defer service.Close()

	d := extensiontest.Daemon(t, append([]extensiontest.Option{extensiontest.Package("testdata/weather"),
		extensiontest.Secret("api_key", "test-key"),
		extensiontest.Entry("weather", map[string]any{"service": service.URL})}, options...)...)
	agent := d.Agent().
		Call("weather.forecast", map[string]any{"city": "Leeds"}).
		Call("weather.forecast", map[string]any{}).
		Call("weather.report", map[string]any{"summary": "rain"}).
		Ends(end)

	work := d.Start("weather.check", nil)
	if r := agent.Played(1).Results[0]; r.IsError || !strings.Contains(r.Text, "rain in Leeds") {
		t.Fatalf("forecast: %+v", r)
	}
	if r := agent.Played(1).Results[1]; !r.IsError || !strings.Contains(r.Text, "a forecast needs a city") {
		t.Fatalf("a forecast without a city: %+v", r)
	}
	d.WaitWork(work.ID, "completed")
	if reported := d.Events("weather.reported"); len(reported) != 1 || !strings.Contains(string(reported[0].Data.Data), `"summary":"rain"`) {
		t.Fatalf("reported: %+v", reported)
	}
}
