package protocol_test

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// The TypeScript SDK's wire messages (sdk/typescript/src/wire.ts) are
// schemas written by hand. Each names the Go type it encodes, and every
// field must be one of that type's JSON fields, so renaming or removing a
// Go field fails here rather than in a TypeScript extension at run time. A
// field the TypeScript side requires must not be omitempty in Go, since Go
// may leave it out; and in a message the SDK decodes, a Go slice, map or
// pointer that isn't omitempty must be nullable, since Go sends nil as null.
var typescriptWire = map[string]any{
	"Payload":              protocol.Payload{},
	"Key":                  protocol.Key{},
	"Event":                protocol.Event{},
	"AcceptedEvent":        protocol.AcceptedEvent{},
	"DisplayLink":          protocol.DisplayLink{},
	"Display":              protocol.Display{},
	"Work":                 protocol.Work{},
	"LeaseRef":             protocol.LeaseRef{},
	"Authority":            protocol.Authority{},
	"HostInfo":             protocol.HostInfo{},
	"InitializeParams":     protocol.InitializeParams{},
	"InitializeResult":     protocol.InitializeResult{},
	"ConnectionEntry":      protocol.ConnectionEntry{},
	"ConfigEntry":          protocol.ConfigEntry{},
	"KVState":              protocol.KVState{},
	"ConfigureParams":      protocol.ConfigureParams{},
	"ConfigureResult":      protocol.ConfigureResult{},
	"ValidateParams":       protocol.ValidateParams{},
	"OperationParams":      protocol.OperationParams{},
	"OperationResult":      protocol.OperationResult{},
	"TaskRunParams":        protocol.TaskRunParams{},
	"CheckRunParams":       protocol.CheckRunParams{},
	"CheckRunResult":       protocol.CheckRunResult{},
	"KVGetParams":          protocol.KVGetParams{},
	"KVEntry":              protocol.KVEntry{},
	"KVGetResult":          protocol.KVGetResult{},
	"KVListParams":         protocol.KVListParams{},
	"KVListResult":         protocol.KVListResult{},
	"KVCheck":              protocol.KVCheck{},
	"KVPut":                protocol.KVPut{},
	"KVDelete":             protocol.KVDelete{},
	"KVCommitParams":       protocol.KVCommitParams{},
	"StateParams":          protocol.StateParams{},
	"EventAcceptParams":    protocol.EventAcceptParams{},
	"EventAcceptResult":    protocol.EventAcceptResult{},
	"SecretResolveParams":  protocol.SecretResolveParams{},
	"SecretResolveResult":  protocol.SecretResolveResult{},
	"ArtifactCreateParams": protocol.ArtifactCreateParams{},
	"ArtifactCreateResult": protocol.ArtifactCreateResult{},
	"ArtifactWriteParams":  protocol.ArtifactWriteParams{},
	"ArtifactTarget":       protocol.ArtifactTarget{},
	"ArtifactRef":          protocol.ArtifactRef{},
	"Family":               protocol.Family{},
	"ExtensionDecl":        protocol.ExtensionDecl{},
	"OperationDecl":        protocol.OperationDecl{},
	"TaskDecl":             protocol.TaskDecl{},
	"CheckDecl":            protocol.CheckDecl{},
	"Provides":             protocol.Provides{},
	"Manifest":             protocol.Manifest{},
	"CertificationFile":    protocol.CertificationFile{},
	"Fixture":              protocol.Fixture{},
	"OperationCase":        protocol.OperationCase{},
}

// Messages the SDK decodes (the host sends them); the rest it only sends.
var typescriptInbound = map[string]bool{
	"Payload": true, "Key": true, "Event": true, "AcceptedEvent": true, "DisplayLink": true, "Display": true, "Work": true,
	"LeaseRef": true, "Authority": true, "HostInfo": true, "InitializeParams": true, "ConnectionEntry": true, "ConfigEntry": true,
	"KVState": true, "ConfigureParams": true, "ValidateParams": true, "OperationParams": true, "TaskRunParams": true,
	"CheckRunParams": true, "KVEntry": true, "KVGetResult": true, "KVListResult": true, "EventAcceptResult": true,
	"SecretResolveResult": true, "ArtifactCreateResult": true, "ArtifactRef": true,
}

var (
	tsMessage = regexp.MustCompile(`(?m)^export const (\w+) = t\.object\(\{\n((?:.*\n)*?)\}, open\);`)
	tsField   = regexp.MustCompile(`(?m)^  (\w+): (.*)$`)
)

func TestTypeScriptWireMatchesGo(t *testing.T) {
	src, err := os.ReadFile("../sdk/typescript/src/wire.ts")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range tsMessage.FindAllStringSubmatch(string(src), -1) {
		name, body := m[1], m[2]
		seen[name] = true
		goValue, ok := typescriptWire[name]
		if !ok {
			t.Errorf("wire.ts message %s names no Go type: add it to typescriptWire", name)
			continue
		}
		fields := jsonFields(reflect.TypeOf(goValue))
		for _, f := range tsField.FindAllStringSubmatch(body, -1) {
			field, schema := f[1], f[2]
			optional := strings.HasPrefix(schema, "t.optional(")
			inner := strings.TrimPrefix(schema, "t.optional(")
			nullable := strings.HasPrefix(inner, "t.nullable(") || strings.HasPrefix(inner, "t.any(")
			g, ok := fields[field]
			switch {
			case !ok:
				t.Errorf("wire.ts %s.%s: %T has no JSON field %q", name, field, goValue, field)
			case !optional && g.omitempty:
				t.Errorf("wire.ts %s.%s is required, but %T omits it when empty: make it t.optional", name, field, goValue)
			case typescriptInbound[name] && !optional && g.nilable && !nullable:
				t.Errorf("wire.ts %s.%s: %T can send it as null: make it t.nullable", name, field, goValue)
			}
		}
	}
	var missing []string
	for name := range typescriptWire {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("typescriptWire names messages wire.ts no longer has: %v", missing)
	}
}

type jsonField struct{ omitempty, nilable bool }

// jsonFields maps a struct's JSON field names to how Go encodes them.
func jsonFields(t reflect.Type) map[string]jsonField {
	out := map[string]jsonField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			for k, v := range jsonFields(f.Type) {
				out[k] = v
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		kind := f.Type.Kind()
		out[name] = jsonField{
			omitempty: strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero"),
			nilable:   kind == reflect.Slice || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Interface,
		}
	}
	return out
}
