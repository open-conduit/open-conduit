package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// ForecastRequest is weather.forecast's input.
type ForecastRequest struct {
	City string `json:"city" doc:"The city to forecast, by name."`
	Days int    `json:"days,omitempty"`
}

// Forecast is weather.forecast's output.
type Forecast struct {
	City    string   `json:"city"`
	Summary []string `json:"summary"`
}

func forecast(_ context.Context, _ extension.Authority, in ForecastRequest) (Forecast, error) {
	if in.City == "" {
		return Forecast{}, &extension.OperationError{Code: "bad_request", Message: "name a city"}
	}
	days := max(in.Days, 1)
	out := Forecast{City: in.City}
	for range days {
		out.Summary = append(out.Summary, "sunny")
	}
	return out, nil
}

// weather is a minimal extension declared once: one operation an agent
// calls.
func weather() *sdk.Extension {
	return sdk.New("weather", sdk.Version("0.1.0"), sdk.Summary("Forecasts for a city.")).
		Operation("weather.forecast", sdk.Handler(forecast), sdk.Guidance("Call `weather.forecast` with a city before planning anything outdoors."))
}

func Example() {
	ext := weather()
	if sdk.PackageCommand(os.Args[1:], ext.Manifest, nil, os.Stdout) {
		return // package-manifest
	}
	// In main: _ = sdk.Serve(ext)
	m, _ := json.MarshalIndent(ext.Manifest, "", "  ")
	fmt.Println(string(m))
	// Output:
	// {
	//   "protocol": [
	//     1
	//   ],
	//   "name": "weather",
	//   "version": "0.1.0",
	//   "summary": "Forecasts for a city.",
	//   "command": [
	//     "weather-extension"
	//   ],
	//   "provides": {
	//     "operations": [
	//       {
	//         "name": "weather.forecast",
	//         "kind": "handler",
	//         "guidance": "Call `weather.forecast` with a city before planning anything outdoors.",
	//         "input": {
	//           "additionalProperties": false,
	//           "properties": {
	//             "city": {
	//               "description": "The city to forecast, by name.",
	//               "type": "string"
	//             },
	//             "days": {
	//               "type": "integer"
	//             }
	//           },
	//           "required": [
	//             "city"
	//           ],
	//           "type": "object"
	//         }
	//       }
	//     ]
	//   }
	// }
}

func TestDeclaredOperationIsServedTyped(t *testing.T) {
	ctx := context.Background()
	h := sdktest.New(t, weather())
	got, err := sdktest.Call[Forecast](ctx, h, "weather.forecast", ForecastRequest{City: "Leeds", Days: 2})
	if err != nil || got.City != "Leeds" || len(got.Summary) != 2 {
		t.Fatalf("forecast: %+v %v", got, err)
	}
	res, err := sdktest.Call[sdktest.OperationResult](ctx, h, "weather.forecast", ForecastRequest{City: "Leeds"})
	if err != nil || res.Output.Schema != "weather.forecast.v1" {
		t.Fatalf("output schema: %+v %v", res, err)
	}
	for _, c := range []struct{ in, want string }{
		{`{"days":2}`, `weather.forecast: missing field "city"`},
		{``, `weather.forecast: missing field "city"`},
		{`{"city":"Leeds","when":"now"}`, `weather.forecast: unknown field "when"`},
		{`{"city":3}`, `weather.forecast: field "city" has the wrong type (got a JSON number)`},
		{`["Leeds"]`, `weather.forecast: the input must be a JSON object`},
	} {
		_, err := h.Operation(ctx, "weather.forecast", domain.Payload{Data: json.RawMessage(c.in)})
		var oe *extension.OperationError
		if !errors.As(err, &oe) || oe.Code != "bad_request" || oe.Message != c.want {
			t.Errorf("input %s: %v, want bad_request %q", c.in, err, c.want)
		}
	}
}

type sampler struct{}

func (sampler) Match(context.Context, extension.RouteInput) (extension.RouteDecision, error) {
	return extension.RouteDecision{}, nil
}
func (sampler) Samples(context.Context, extension.SampleInput) []extension.RouteSample { return nil }

type plain struct{}

func (plain) Match(context.Context, extension.RouteInput) (extension.RouteDecision, error) {
	return extension.RouteDecision{}, nil
}

type ingress struct{}

func (ingress) Spec() extension.IngressSpec { return extension.IngressSpec{} }
func (ingress) Push(context.Context, extension.Source, extension.Delivery) (extension.PushResult, error) {
	return extension.PushResult{}, nil
}
func (ingress) Reconcile(context.Context, extension.Scan) (extension.Snapshot, error) {
	return extension.Snapshot{}, nil
}

// Each registration sets its handler and the declaration that enables it.
func TestRegistrationsDeriveTheManifest(t *testing.T) {
	noop := func(context.Context, *sdk.Host) error { return nil }
	ok := func(context.Context, *sdk.Host, bool) (string, error) { return "ok", nil }
	lookup := func(name string) (extension.Ingress, bool) { return ingress{}, name == "acme/s1" }
	ext := sdk.New("acme", sdk.Version("1.2.0"), sdk.Command("bin/acme"), sdk.KVVersion(2), sdk.Permissions(protocol.PermissionProbes)).
		IngressType("acme", lookup, protocol.Family{Type: "acme.item.changed", ReDerivable: true}).
		Family(protocol.Family{Type: "acme.item.noted", ReDerivable: true}).
		Route("acme", sampler{}, sdk.RouteBinding()).
		Route("acme-plain", plain{}, sdk.RouteConfig()).
		WorkKind("acme.item", sdk.Trigger(func(in []domain.Event) int { return len(in) - 1 })).
		WorkKind("acme.digest", sdk.Standing()).
		Recurrence("acme.daily", func([]byte, domain.Timer, time.Time) (*domain.Event, *domain.Timer, error) { return nil, nil, nil }).
		Operation("acme.lookup", sdk.Handler(func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
			return domain.Payload{}, nil
		}), sdk.OutputSchema("acme.lookup.result.v1")).
		Operation("acme.await", sdk.Wait(func(context.Context, extension.Authority, struct{}) (extension.WaitPlan, error) {
			return extension.WaitPlan{}, nil
		})).
		Operation("acme.note", sdk.Emit(func(context.Context, extension.Authority, struct {
			Text string `json:"text"`
		}) (extension.EmitPlan, error) {
			return extension.EmitPlan{}, nil
		}), sdk.Commit()).
		DependencyFilter(func(domain.Payload, domain.Event) bool { return true }).
		Task("acme-sync", time.Minute, noop).
		Check("acme", ok, sdk.Deep())
	want := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.2.0", Command: []string{"bin/acme"}, KVVersion: 2, Permissions: []string{protocol.PermissionProbes}, Provides: protocol.Provides{
		Extensions:  []protocol.ExtensionDecl{{Type: "acme", Ingress: true}},
		Families:    []protocol.Family{{Type: "acme.item.changed", ReDerivable: true}, {Type: "acme.item.noted", ReDerivable: true}},
		Routes:      []protocol.RouteDecl{{Handler: "acme", Binding: true, Samples: true}, {Handler: "acme-plain", Config: true}},
		WorkKinds:   []protocol.WorkKindDecl{{Kind: "acme.item", Trigger: true}, {Kind: "acme.digest", Standing: true}},
		Recurrences: []string{"acme.daily"},
		Operations: []protocol.OperationDecl{
			{Name: "acme.lookup", Kind: protocol.OperationHandler},
			{Name: "acme.await", Kind: protocol.OperationWait, Input: json.RawMessage(`{"additionalProperties":false,"properties":{},"type":"object"}`)},
			{Name: "acme.note", Kind: protocol.OperationEmit, Commit: true, Input: json.RawMessage(`{"additionalProperties":false,"properties":{"text":{"type":"string"}},"required":["text"],"type":"object"}`)},
		},
		DependencyFilter: true,
		Tasks:            []protocol.TaskDecl{{Name: "acme-sync", IntervalSeconds: 60}},
		Checks:           []protocol.CheckDecl{{Name: "acme", Deep: true}},
	}}
	if !reflect.DeepEqual(ext.Manifest, want) {
		got, _ := json.Marshal(ext.Manifest)
		w, _ := json.Marshal(want)
		t.Fatalf("manifest\n%s\nwant\n%s", got, w)
	}
	if err := ext.Manifest.Validate(); err != nil {
		t.Fatalf("derived manifest invalid: %v", err)
	}
	if len(ext.Operations) != 3 || ext.Operations[0].Handler == nil || ext.Operations[1].Wait == nil || ext.Operations[2].Emit == nil ||
		ext.Routes["acme"] == nil || ext.Routes["acme-plain"] == nil || ext.Triggers["acme.item"] == nil || ext.Recurrences["acme.daily"] == nil ||
		ext.Filter == nil || ext.Tasks["acme-sync"] == nil || ext.Checks["acme"] == nil {
		t.Fatalf("handlers not registered: %+v", ext)
	}
	if in, found := ext.Ingress("acme/s1"); !found || in == nil {
		t.Fatal("ingress lookup not registered")
	}
	if _, found := ext.Ingress("other"); found {
		t.Fatal("ingress lookup found an unknown source")
	}
}

func TestMisdeclaredOperationsPanic(t *testing.T) {
	for name, f := range map[string]func(){
		"commit on a handler": func() {
			sdk.New("acme").Operation("acme.x", sdk.Handler(func(context.Context, extension.Authority, struct{}) (struct{}, error) { return struct{}{}, nil }), sdk.Commit())
		},
		"input that is not an object": func() {
			sdk.Handler(func(context.Context, extension.Authority, string) (struct{}, error) { return struct{}{}, nil })
		},
		"no handler": func() { sdk.New("acme").Operation("acme.x", sdk.Op{}) },
	} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.HasPrefix(fmt.Sprint(r), "sdk: ") {
					t.Errorf("%s: panic %v", name, r)
				}
			}()
			f()
		}()
	}
}

func TestSchemaOf(t *testing.T) {
	type Inner struct {
		N float64 `json:"n"`
	}
	type Embedded struct {
		Flat string `json:"flat"`
	}
	type Tree struct {
		Kids []Tree `json:"kids,omitempty"`
	}
	type All struct {
		Embedded
		B       bool              `json:"b"`
		I       int64             `json:"i,omitempty"`
		P       *string           `json:"p"`
		At      time.Time         `json:"at,omitzero"`
		Raw     json.RawMessage   `json:"raw,omitempty"`
		Bytes   []byte            `json:"bytes,omitempty"`
		Tags    map[string]string `json:"tags,omitempty"`
		Inner   Inner             `json:"inner"`
		Tree    Tree              `json:"tree,omitempty"`
		Default string
		Skip    string `json:"-"`
		hidden  string
	}
	got, _ := json.Marshal(sdk.SchemaOf(reflect.TypeFor[All]()))
	want := `{"additionalProperties":false,"properties":{` +
		`"Default":{"type":"string"},` +
		`"at":{"format":"date-time","type":"string"},` +
		`"b":{"type":"boolean"},` +
		`"bytes":{"contentEncoding":"base64","type":"string"},` +
		`"flat":{"type":"string"},` +
		`"i":{"type":"integer"},` +
		`"inner":{"additionalProperties":false,"properties":{"n":{"type":"number"}},"required":["n"],"type":"object"},` +
		`"p":{"type":"string"},` +
		`"raw":{},` +
		`"tags":{"additionalProperties":{"type":"string"},"type":"object"},` +
		`"tree":{"additionalProperties":false,"properties":{"kids":{"items":{"type":"object"},"type":"array"}},"type":"object"}` +
		`},"required":["flat","b","inner","Default"],"type":"object"}`
	if string(got) != want {
		t.Fatalf("schema\n%s\nwant\n%s", got, want)
	}
	_ = All{}.hidden
}
