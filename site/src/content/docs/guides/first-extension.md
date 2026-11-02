---
title: Build your first extension
description: In about 30 minutes, write a weather extension that agents can call, test it, install it and watch an agent use it.
---

This tutorial builds a small extension package from nothing: `weather`, which gives agents a forecast for a city by asking a weather service. On the way you write a toolbelt operation, test it on its own and inside a real daemon, check the package and install it. It takes about 30 minutes.

You need [Go](https://go.dev/dl/) 1.24.2 or later and Make. For the last step you also need Open Conduit installed and set up with a GitHub repository, as in the [quick start](/open-conduit/start/quick-start/); the earlier steps don't need a daemon at all.

## The words you'll meet

- **Operation:** something an agent can ask Open Conduit to do, such as `github.issue.comment`. Agents call operations through their toolbelt; your extension adds one, `weather.forecast`.
- **Event:** something that happened, recorded once: an issue opened, a comment posted. Events start and wake Work.
- **Work:** one job an agent does, such as working an issue, across as many turns as it takes.
- **Scope:** one part of your configuration, such as a GitHub repository, with its own agents, their operations and the extensions they use.
- **Entry:** an extension's settings in one scope. Your weather entry names the weather service that scope uses.
- **Binding:** an agent a scope can run, and the operations it may call.

## 1. Start the package

`extension new` writes a package repository whose checks pass on its first run:

```sh
open-conduit extension new weather
cd weather
make check
```

`make check` formats, vets and tests the package, builds it into `dist/weather` and lints it. The repository holds one operation, `weather.hello`, which you are about to replace:

| File | Holds |
| --- | --- |
| `main.go` | the extension: its operations, and the fixture certification runs |
| `main_test.go` | unit tests, which call the operation in memory |
| `e2e_test.go` | an end-to-end test, which installs the package in a real daemon and has a scripted agent call it |
| `playbooks/weather.md` | what the package tells agents about itself |
| `Makefile` | `check`, `package`, `lint` and `certify` |

## 2. Write `weather.forecast`

An operation is a Go function from an input struct to an output struct. The struct's fields become the operation's input schema, which agents read, so a field's `doc` tag is the description an agent sees.

This operation needs to know where the weather service is, and that differs from one setup to another. So the package also declares an extension type, `weather`: an operator adds a weather entry to a scope with the service's address, and Open Conduit sends every entry to the package in `configure`, at start and whenever the configuration changes. The handler looks up the service for the scope the calling agent works in.

Replace `main.go` with:

```go title="main.go"
// Command weather-extension is an Open Conduit extension package. It
// serves one toolbelt operation, weather.forecast, that asks the weather
// service a scope's weather entry names for a city's forecast.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// version is the package's release, set by `make package VERSION=...`.
var version = "0.1.0"

// ForecastInput is what an agent sends.
type ForecastInput struct {
	City string `json:"city" doc:"The city to forecast, by name."`
}

// ForecastOutput is what the agent gets back: the service's answer.
type ForecastOutput struct {
	City    string `json:"city"`
	Summary string `json:"summary"`
	HighC   int    `json:"high_c"`
	LowC    int    `json:"low_c"`
}

// EntryConfig is the config of a weather entry in a scope.
type EntryConfig struct {
	Service string `json:"service"`
}

// services maps each scope to the service its weather entry names. Open
// Conduit sends every entry in configure, at start and on each config
// change.
var (
	mu       sync.Mutex
	services = map[string]string{}
)

func configure(_ context.Context, _ *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	next := map[string]string{}
	for _, e := range p.Entries {
		var cfg EntryConfig
		if err := json.Unmarshal(e.Config, &cfg); err != nil {
			return protocol.ConfigureResult{}, fmt.Errorf("weather entry %s: %w", e.Name, err)
		}
		next[e.ScopeID] = strings.TrimSuffix(cfg.Service, "/")
	}
	mu.Lock()
	services = next
	mu.Unlock()
	return protocol.ConfigureResult{}, nil
}

func forecast(ctx context.Context, a extension.Authority, in ForecastInput) (ForecastOutput, error) {
	city := strings.TrimSpace(in.City)
	if city == "" {
		return ForecastOutput{}, &extension.OperationError{Code: "bad_request", Message: "weather.forecast needs a city"}
	}
	mu.Lock()
	service := services[string(a.ScopeID)]
	mu.Unlock()
	if service == "" {
		return ForecastOutput{}, &extension.OperationError{Code: "not_configured", Message: "this scope has no weather entry naming a service"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, service+"/forecast?city="+url.QueryEscape(city), nil)
	if err != nil {
		return ForecastOutput{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ForecastOutput{}, &extension.OperationError{Code: "service_unavailable", Message: "the weather service did not answer; try again later"}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ForecastOutput{}, &extension.OperationError{Code: "unknown_city", Message: "the weather service doesn't know " + city}
	case resp.StatusCode != http.StatusOK:
		return ForecastOutput{}, &extension.OperationError{Code: "service_unavailable", Message: fmt.Sprintf("the weather service answered %d", resp.StatusCode)}
	}
	var out ForecastOutput
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ForecastOutput{}, fmt.Errorf("the weather service's answer: %w", err)
	}
	return out, nil
}

// build is the extension. Each registration also declares itself, so the
// manifest (`package-manifest`) is derived from what is registered here.
func build() *sdk.Extension {
	ext := sdk.New("weather", sdk.Version(version), sdk.Summary("weather: forecasts for agents."))
	ext.Manifest.Provides.Playbooks = "playbooks"
	ext.Manifest.Provides.Extensions = []protocol.ExtensionDecl{{Type: "weather"}}
	ext.Configure = configure
	return ext.Operation("weather.forecast", sdk.Handler(forecast),
		sdk.Guidance("Call `weather.forecast` with a `city` to get its forecast: a summary and the high and low in °C."))
}

// certification is the fixture `open-conduit extension certify` runs the
// package against: one case per operation, with the output it must give.
func certification() protocol.CertificationFile {
	return protocol.CertificationFile{Fixture: &protocol.Fixture{Operations: []protocol.OperationCase{
		{Operation: "weather.forecast", Input: protocol.Payload{Data: json.RawMessage(`{"city":" "}`)}, Error: "bad_request"},
		{Operation: "weather.forecast", Input: protocol.Payload{Data: json.RawMessage(`{"city":"Leeds"}`)}, Error: "not_configured"},
	}}}
}

func main() {
	ext := build()
	if sdk.PackageCommand(os.Args[1:], ext.Manifest, certification(), os.Stdout) {
		return
	}
	if err := sdk.Serve(ext); err != nil {
		os.Exit(1)
	}
}
```

What changed from the scaffold:

- **The operation.** `forecast` checks its input, finds the scope's service and calls `GET /forecast?city=NAME` on it. A refusal the agent can act on is an `extension.OperationError` with a stable code and a short message; the agent sees both.
- **The entry type.** `Provides.Extensions` declares the `weather` type, and `configure` keeps each scope's service.
- **The guidance.** `sdk.Guidance` tells agents when to call the operation. It is all an agent knows about it, so say what it gives back.
- **The certification fixture.** `certification()` lists cases `make certify` checks. Certification never contacts a real service, so these cases are the refusals the package gives without one.

Update the playbook the same way. Replace `playbooks/weather.md` with:

```md title="playbooks/weather.md"
---
name: weather
description: How an agent gets a weather forecast through the weather package. Read it before planning anything that depends on the weather.
---

# weather

The weather package gives you forecasts.

- Call `weather.forecast` with a `city` to get its forecast: a summary and the high and low in °C.
- If it refuses with `unknown_city`, ask for a nearby larger city instead of guessing.
```

## 3. Test it

### In memory, with `sdktest`

`sdktest` runs your extension on an in-memory host, over the same protocol the daemon uses, so a test runs in milliseconds and needs nothing built. The tests stand up a fake weather service with Go's `httptest`, configure a weather entry naming it, and call the operation. Replace `main_test.go` with:

```go title="main_test.go"
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// fakeService is a weather service that knows one city, Leeds.
func fakeService(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/forecast" || r.URL.Query().Get("city") != "Leeds" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ForecastOutput{City: "Leeds", Summary: "rain", HighC: 12, LowC: 7})
	}))
	t.Cleanup(s.Close)
	return s
}

// configured is the extension on an in-memory host, configured with one
// weather entry in scope "test" naming the service.
func configured(t *testing.T, service string) *sdktest.Host {
	t.Helper()
	h := sdktest.New(t, build())
	cfg, _ := json.Marshal(EntryConfig{Service: service})
	entry := protocol.ConfigEntry{ScopeID: "test", Type: "weather", Name: "weather", Config: cfg}
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{entry}}); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestForecast(t *testing.T) {
	h := configured(t, fakeService(t).URL)
	got, err := sdktest.Call[ForecastOutput](context.Background(), h, "weather.forecast", ForecastInput{City: "Leeds"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "rain" || got.HighC != 12 {
		t.Fatalf("forecast %+v", got)
	}
}

func TestForecastRefusals(t *testing.T) {
	h := configured(t, fakeService(t).URL)
	for city, code := range map[string]string{"": "bad_request", "Atlantis": "unknown_city"} {
		_, err := sdktest.Call[ForecastOutput](context.Background(), h, "weather.forecast", ForecastInput{City: city})
		var refused *extension.OperationError
		if !errors.As(err, &refused) || refused.Code != code {
			t.Errorf("city %q: got %v, want %s", city, err, code)
		}
	}
}
```

Run them:

```sh
go test -run 'TestForecast' ./...
```

### End to end, with `extensiontest`

`extensiontest` answers the question you really have: when an agent calls my operation, does it work? It builds the package, installs it in a real Open Conduit daemon with a scope made from your manifest, and plays a scripted agent that calls operations through its toolbelt the way Claude Code does. `extensiontest.Entry` gives the generated weather entry its config, here the fake service's address. Replace `e2e_test.go` with:

```go title="e2e_test.go"
//go:build unix

package main

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extensiontest"
)

// TestAgentAsksForAForecast builds this package, installs it in a real
// Open Conduit daemon with a weather entry naming the fake service, and has
// a scripted agent call weather.forecast through its toolbelt.
func TestAgentAsksForAForecast(t *testing.T) {
	service := fakeService(t)
	d := extensiontest.Daemon(t, extensiontest.Package("."),
		extensiontest.Entry("weather", EntryConfig{Service: service.URL}))
	agent := d.Agent().
		Call("weather.forecast", map[string]any{"city": "Leeds"}).
		Ends("finished")

	work := d.Start("weather.try", nil)
	if r := agent.Result(1, "weather.forecast"); r.IsError || !strings.Contains(r.Text, "rain") {
		t.Fatalf("weather.forecast: %+v", r)
	}
	d.WaitWork(work.ID, "completed")
}
```

The scripted agent plays one turn: it calls `weather.forecast` for Leeds and ends. The test starts Work for it, checks what the call returned, and waits for the Work to complete. Run everything, as CI will:

```sh
make check
```

The first run builds the daemon, so it takes a minute; later runs reuse the build.

## 4. Check, install and use it

`make check` ended by linting the package in `dist/weather`. You can lint it yourself, and run the certification suite:

```sh
open-conduit extension check dist/weather
make certify
```

`extension check` reads the manifest and the playbooks without starting the package, and says what to change for each problem it finds. `make certify` starts the package and checks it speaks the protocol correctly, using the fixture in `certification()`.

### A weather service to try it with

To see a real agent use the operation, it needs a weather service to ask. Add a stand-in that gives every city the same forecast, as `fakeweather/main.go`:

```go title="fakeweather/main.go"
// Command fakeweather is a stand-in weather service for trying the package
// by hand: it answers GET /forecast?city=NAME for any city.
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("GET /forecast", func(w http.ResponseWriter, r *http.Request) {
		city := r.URL.Query().Get("city")
		_ = json.NewEncoder(w).Encode(map[string]any{"city": city, "summary": "sunny spells", "high_c": 18, "low_c": 9})
	})
	log.Println("fake weather service on http://127.0.0.1:8090")
	log.Fatal(http.ListenAndServe("127.0.0.1:8090", nil))
}
```

Start it in a terminal of its own and leave it running:

```sh
go run ./fakeweather
```

### Watch it run

Before you install anything, `open-conduit dev` runs the package against a throwaway daemon, with a scope and an agent made from your manifest, the same way `extensiontest` does. From the package's root, in another terminal, give the weather entry the stand-in's address:

```sh
open-conduit dev --entry 'weather={"service":"http://127.0.0.1:8090"}'
```

It builds the package, starts the daemon and prints the web view's URL and a `work create` command. Open the URL, run the command to start a Work, and watch the agent call `weather.forecast` and get back sunny spells. The agent is Claude Code, with your own sign-in.

Leave `dev` running and change the forecast handler, for example the message for an empty city. When you save, `dev` rebuilds the package and restarts the daemon; earlier Work stays. A build that fails is reported, and the running daemon carries on as it was. Press Ctrl-C to stop it: it stops the daemon and removes its state, so nothing is left behind.

[Running a package while you build it](/open-conduit/architecture/extension-packaging/#running-a-package-while-you-build-it-open-conduit-dev) covers its flags and what it prints.

### Install the package

Install the package and restart the daemon, which reads packages only when it starts:

```sh
open-conduit extension install dist/weather
open-conduit service stop
open-conduit service start
```

If you run the daemon in the foreground instead, stop `open-conduit serve` and start it again.

### Give a scope the operation

Your scope needs a weather entry, and its agent needs to be allowed to call `weather.forecast`. The commands below use the scope `web` and its binding `agent`, as `open-conduit init github --repo acme/web` names them; use your own scope's ID. Each command checks the result before saving it:

```sh
open-conduit config set /scopes/web/extensions/- '{"name": "weather", "type": "weather", "secret_names": [], "config": {"service": "http://127.0.0.1:8090"}}'
open-conduit config set /scopes/web/authorization/operations/- '"weather.forecast"'
open-conduit config set /scopes/web/bindings/agent/operations/- '"weather.forecast"'
```

The first adds the entry. The second lets the scope use the operation at all, and the third lets its agent call it. Then hand the changed file to the running daemon:

```sh
open-conduit config apply "$(open-conduit config path)"
```

### Ask an agent

Open an issue in the repository: "What's the weather forecast for Leeds? Answer here, no PR needed." Once Open Conduit picks it up, as it does any issue in that scope (`open-conduit claim N` claims issue N by hand), the agent finds `weather.forecast` in its toolbelt with your guidance. It calls it and answers on the issue with what the stand-in service said: sunny spells, 18°C at most.

Watch it happen in `open-conduit tui`, or find the run and read the agent's transcript, where the call and its result appear:

```sh
open-conduit runs list --scope web
open-conduit runs log ID --transcript
```

## Where next

- **Publish it.** Change the module path in `go.mod` to your repository's, push it, and CI (`.github/workflows/ci.yml`) runs `make check` and certifies the package against Open Conduit's `main` and its latest release. `make package VERSION=0.2.0` builds a release; [installing and upgrading](/open-conduit/architecture/extension-packaging/#install-update-rollback-and-removal) covers release archives.
- **Use a secret.** A real weather service wants an API key. Select a secret in the entry's `secret_names`, read it in the handler with `sdk.HostFrom(ctx).Secrets`, and give it to the tests with `extensiontest.Secret` and `sdktest.WithSecrets`.
- **Add more.** An extension can also receive webhooks, route Events to agents, record Events of its own and run tasks. [Writing an extension with the SDK](/open-conduit/architecture/extension-packaging/#writing-an-extension-with-the-sdk) lists every surface, and [testing a package](/open-conduit/architecture/extension-packaging/#testing-a-package) covers both harnesses in full.
- **The reference.** [Extension packaging, SDK and certification](/open-conduit/architecture/extension-packaging/) and [the extension protocol](/open-conduit/architecture/extension-protocol/) are the full references; [troubleshooting](/open-conduit/architecture/extension-packaging/#troubleshooting) says what each refusal code means.
