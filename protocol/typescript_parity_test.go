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

// The TypeScript SDK's wire types (sdk/typescript/src/wire.ts) are written
// by hand. Each interface there names the Go type it encodes, and every
// field must be one of that type's JSON fields, so renaming or removing a
// Go field fails here rather than in a TypeScript extension at run time. A
// field the TypeScript side requires must not be omitempty in Go, since the
// Go side may leave it out.
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
	"KVEntryWire":          protocol.KVEntry{},
	"KVGetResult":          protocol.KVGetResult{},
	"KVListParams":         protocol.KVListParams{},
	"KVListResult":         protocol.KVListResult{},
	"KVCheck":              protocol.KVCheck{},
	"KVPutWire":            protocol.KVPut{},
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

var (
	tsInterface = regexp.MustCompile(`(?m)^export interface (\w+)(?: extends [\w, ]+)? \{\n((?:.*\n)*?)\}`)
	tsField     = regexp.MustCompile(`(?m)^  (\w+)(\??): `)
)

func TestTypeScriptWireMatchesGo(t *testing.T) {
	src, err := os.ReadFile("../sdk/typescript/src/wire.ts")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range tsInterface.FindAllStringSubmatch(string(src), -1) {
		name, body := m[1], m[2]
		seen[name] = true
		goValue, ok := typescriptWire[name]
		if !ok {
			t.Errorf("wire.ts interface %s names no Go type: add it to typescriptWire", name)
			continue
		}
		fields := jsonFields(reflect.TypeOf(goValue))
		for _, f := range tsField.FindAllStringSubmatch(body, -1) {
			omitempty, ok := fields[f[1]]
			switch {
			case !ok:
				t.Errorf("wire.ts %s.%s: %T has no JSON field %q", name, f[1], goValue, f[1])
			case f[2] == "" && omitempty:
				t.Errorf("wire.ts %s.%s is required, but %T omits it when empty: make it optional", name, f[1], goValue)
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
		t.Errorf("typescriptWire names interfaces wire.ts no longer has: %v", missing)
	}
}

// jsonFields maps a struct's JSON field names to whether they are omitempty.
func jsonFields(t reflect.Type) map[string]bool {
	out := map[string]bool{}
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
		out[name] = strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
	}
	return out
}
