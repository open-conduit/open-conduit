package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// Declaring each surface once.
//
// New starts an Extension whose manifest is derived from what is registered
// on it: Operation, Route, WorkKind, Task, Check, IngressType, Recurrence,
// Family and DependencyFilter each set the handler and add the declaration that
// enables it, so the two cannot disagree. Hand-set what cannot be inferred
// (permissions, kv_version, the surfaces with no registration method) on
// Manifest or with an Option; a literal Extension keeps working unchanged.
// A registration that cannot be served panics, as http.HandleFunc does.

// Option sets a manifest field New cannot infer.
type Option func(*protocol.Manifest)

// Version sets the manifest's version (required).
func Version(v string) Option { return func(m *protocol.Manifest) { m.Version = v } }

// Summary sets the manifest's one-line summary.
func Summary(s string) Option { return func(m *protocol.Manifest) { m.Summary = s } }

// Command sets the package's command (default: "<name>-extension").
func Command(argv ...string) Option { return func(m *protocol.Manifest) { m.Command = argv } }

// Permissions adds permissions the extension uses.
func Permissions(p ...string) Option {
	return func(m *protocol.Manifest) { m.Permissions = append(m.Permissions, p...) }
}

// KVVersion sets the layout version of the extension's own KV namespaces.
func KVVersion(v int) Option { return func(m *protocol.Manifest) { m.KVVersion = v } }

// New returns an Extension named name, speaking protocol version 1, whose
// manifest its registrations fill in.
func New(name string, opts ...Option) *Extension {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: name, Command: []string{name + "-extension"}}
	for _, o := range opts {
		o(&m)
	}
	return &Extension{Manifest: m}
}

// Op is a typed toolbelt operation: Handler, Wait or Emit makes one, and
// Extension.Operation registers it under a name.
type Op struct {
	kind  string
	input json.RawMessage
	bind  func(name, output string) extension.Operation
}

// OperationOption adds to an operation's declaration.
type OperationOption func(*operationDecl)

type operationDecl struct {
	protocol.OperationDecl
	output string
}

// Guidance tells an agent when and how to use the operation, in Markdown.
func Guidance(text string) OperationOption {
	return func(d *operationDecl) { d.Guidance = text }
}

// Commit marks an Emit whose plan writes records through EmitPlan.Commit.
func Commit() OperationOption { return func(d *operationDecl) { d.Commit = true } }

// OutputSchema names the payload schema a Handler's typed output is sent
// as (default "<operation>.v1"). An output of type domain.Payload keeps its
// own.
func OutputSchema(schema string) OperationOption {
	return func(d *operationDecl) { d.output = schema }
}

var payloadType = reflect.TypeFor[domain.Payload]()

// Handler makes a request/response operation. The input's data is decoded
// into In strictly: a missing required field (see SchemaOf), a field In
// does not have, or a value of the wrong type refuses the call with
// bad_request before fn runs. Out is encoded as the
// output's data. An In or Out of type domain.Payload passes the payload
// as it is.
func Handler[In, Out any](fn func(context.Context, extension.Authority, In) (Out, error)) Op {
	schema, required := inputSchema[In]()
	return Op{kind: protocol.OperationHandler, input: schema, bind: func(name, output string) extension.Operation {
		return extension.Operation{Name: name, Handler: func(ctx context.Context, a extension.Authority, p domain.Payload) (domain.Payload, error) {
			in, err := decodeInput[In](name, required, p)
			if err != nil {
				return domain.Payload{}, err
			}
			out, err := fn(ctx, a, in)
			if err != nil {
				return domain.Payload{}, err
			}
			return encodeOutput(out, output)
		}}
	}}
}

// Wait makes an operation that suspends the Work (see extension.Operation).
// In is decoded as for Handler.
func Wait[In any](fn func(context.Context, extension.Authority, In) (extension.WaitPlan, error)) Op {
	schema, required := inputSchema[In]()
	return Op{kind: protocol.OperationWait, input: schema, bind: func(name, _ string) extension.Operation {
		return extension.Operation{Name: name, Wait: func(ctx context.Context, a extension.Authority, p domain.Payload) (extension.WaitPlan, error) {
			in, err := decodeInput[In](name, required, p)
			if err != nil {
				return extension.WaitPlan{}, err
			}
			return fn(ctx, a, in)
		}}
	}}
}

// Emit makes an operation that emits one Event of a declared family; with
// the Commit option its plan may write records (see extension.Operation).
// In is decoded as for Handler.
func Emit[In any](fn func(context.Context, extension.Authority, In) (extension.EmitPlan, error)) Op {
	schema, required := inputSchema[In]()
	return Op{kind: protocol.OperationEmit, input: schema, bind: func(name, _ string) extension.Operation {
		return extension.Operation{Name: name, Emit: func(ctx context.Context, a extension.Authority, p domain.Payload) (extension.EmitPlan, error) {
			in, err := decodeInput[In](name, required, p)
			if err != nil {
				return extension.EmitPlan{}, err
			}
			return fn(ctx, a, in)
		}}
	}}
}

// inputSchema is In's JSON Schema and its required top-level fields.
func inputSchema[In any]() (json.RawMessage, []string) {
	t := reflect.TypeFor[In]()
	if t == payloadType {
		return nil, nil
	}
	if t.Kind() != reflect.Struct && !(t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct) {
		panic(fmt.Sprintf("sdk: an operation's input must be a struct or domain.Payload, not %s", t))
	}
	schema := SchemaOf(t)
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	required, _ := schema["required"].([]string)
	return raw, required
}

func decodeInput[In any](name string, required []string, p domain.Payload) (In, error) {
	var in In
	if v, ok := any(&in).(*domain.Payload); ok {
		*v = p
		return in, nil
	}
	data := bytes.TrimSpace(p.Data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		data = []byte("{}")
	}
	var present map[string]json.RawMessage
	if json.Unmarshal(data, &present) != nil {
		return in, &extension.OperationError{Code: "bad_request", Message: name + ": the input must be a JSON object"}
	}
	for _, f := range required {
		if _, ok := present[f]; !ok {
			return in, &extension.OperationError{Code: "bad_request", Message: fmt.Sprintf("%s: missing field %q", name, f)}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, &extension.OperationError{Code: "bad_request", Message: name + ": " + inputError(err)}
	}
	if dec.More() {
		return in, &extension.OperationError{Code: "bad_request", Message: name + ": trailing data after the input"}
	}
	return in, nil
}

// inputError says what is wrong with an input without Go type names.
func inputError(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return fmt.Sprintf("field %q has the wrong type (got a JSON %s)", typeErr.Field, typeErr.Value)
	}
	if s, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return "unknown field " + s
	}
	return "the input is not valid JSON for this operation"
}

func encodeOutput[Out any](out Out, schema string) (domain.Payload, error) {
	if p, ok := any(out).(domain.Payload); ok {
		return p, nil
	}
	data, err := json.Marshal(out)
	if err != nil {
		return domain.Payload{}, err
	}
	return domain.Payload{Schema: schema, Data: data}, nil
}

// Operation registers a toolbelt operation and declares it, with its kind
// and the JSON Schema of its input.
func (e *Extension) Operation(name string, op Op, opts ...OperationOption) *Extension {
	if op.bind == nil {
		panic("sdk: operation " + name + " needs sdk.Handler, sdk.Wait or sdk.Emit")
	}
	d := operationDecl{OperationDecl: protocol.OperationDecl{Name: name, Kind: op.kind, Input: op.input}, output: name + ".v1"}
	for _, o := range opts {
		o(&d)
	}
	if d.Commit && op.kind != protocol.OperationEmit {
		panic("sdk: operation " + name + ": only sdk.Emit operations commit records")
	}
	x := op.bind(name, d.output)
	x.Guidance = d.Guidance
	e.Operations = append(e.Operations, x)
	e.Manifest.Provides.Operations = append(e.Manifest.Provides.Operations, d.OperationDecl)
	return e
}

// RouteOption adds to a route handler's declaration.
type RouteOption func(*protocol.RouteDecl)

// RouteConfig marks a handler whose routes carry config, checked by
// Validate with kind route.
func RouteConfig() RouteOption { return func(d *protocol.RouteDecl) { d.Config = true } }

// RouteBinding marks a handler that checks the binding each of its routes
// binds, through Validate with kind binding.
func RouteBinding() RouteOption { return func(d *protocol.RouteDecl) { d.Binding = true } }

// Route registers a route handler and declares it; a handler that is an
// extension.RouteSampler is declared as offering samples.
func (e *Extension) Route(handler string, h extension.RouteHandler, opts ...RouteOption) *Extension {
	d := protocol.RouteDecl{Handler: handler}
	if _, ok := h.(extension.RouteSampler); ok {
		d.Samples = true
	}
	for _, o := range opts {
		o(&d)
	}
	if e.Routes == nil {
		e.Routes = map[string]extension.RouteHandler{}
	}
	e.Routes[handler] = h
	e.Manifest.Provides.Routes = append(e.Manifest.Provides.Routes, d)
	return e
}

// WorkKindOption adds to a Work kind's declaration.
type WorkKindOption func(*Extension, *protocol.WorkKindDecl)

// Standing marks a kind whose Work stays open across triggers.
func Standing() WorkKindOption {
	return func(_ *Extension, d *protocol.WorkKindDecl) { d.Standing = true }
}

// Trigger selects, from a Work's pending inputs, the index of the one that
// triggers its next execution.
func Trigger(fn func([]domain.Event) int) WorkKindOption {
	return func(e *Extension, d *protocol.WorkKindDecl) {
		d.Trigger = true
		if e.Triggers == nil {
			e.Triggers = map[string]func([]domain.Event) int{}
		}
		e.Triggers[d.Kind] = fn
	}
}

// WorkKind declares a kind of Work the extension's routes create.
func (e *Extension) WorkKind(kind string, opts ...WorkKindOption) *Extension {
	d := protocol.WorkKindDecl{Kind: kind}
	for _, o := range opts {
		o(e, &d)
	}
	e.Manifest.Provides.WorkKinds = append(e.Manifest.Provides.WorkKinds, d)
	return e
}

// Task registers a task the host runs every interval (whole seconds, at
// least one) and declares it.
func (e *Extension) Task(name string, every time.Duration, fn func(context.Context, *Host) error) *Extension {
	if e.Tasks == nil {
		e.Tasks = map[string]func(context.Context, *Host) error{}
	}
	e.Tasks[name] = fn
	e.Manifest.Provides.Tasks = append(e.Manifest.Provides.Tasks, protocol.TaskDecl{Name: name, IntervalSeconds: int(every / time.Second)})
	return e
}

// CheckOption adds to a check's declaration.
type CheckOption func(*protocol.CheckDecl)

// Deep marks a check that also runs a slower, deeper probe on doctor --deep.
func Deep() CheckOption { return func(d *protocol.CheckDecl) { d.Deep = true } }

// Check registers a doctor check and declares it. fn returns a detail line,
// and an error when the check fails.
func (e *Extension) Check(name string, fn func(ctx context.Context, h *Host, deep bool) (string, error), opts ...CheckOption) *Extension {
	d := protocol.CheckDecl{Name: name}
	for _, o := range opts {
		o(&d)
	}
	if e.Checks == nil {
		e.Checks = map[string]func(context.Context, *Host, bool) (string, error){}
	}
	e.Checks[name] = fn
	e.Manifest.Provides.Checks = append(e.Manifest.Provides.Checks, d)
	return e
}

// IngressType declares an extension type whose entries take ingress, with the
// families its sources observe, and registers the lookup of a source the
// last Configure registered by name. Several calls each add a type; a
// source is looked up in each in turn.
func (e *Extension) IngressType(extType string, lookup func(name string) (extension.Ingress, bool), families ...protocol.Family) *Extension {
	prev := e.Ingress
	e.Ingress = func(name string) (extension.Ingress, bool) {
		if prev != nil {
			if in, ok := prev(name); ok {
				return in, true
			}
		}
		return lookup(name)
	}
	e.Manifest.Provides.Extensions = append(e.Manifest.Provides.Extensions, protocol.ExtensionDecl{Type: extType, Ingress: true})
	e.Manifest.Provides.Families = append(e.Manifest.Provides.Families, families...)
	return e
}

// Recurrence registers a calendar recurrence kind and declares it.
func (e *Extension) Recurrence(kind string, fn RecurrenceFunc) *Extension {
	if e.Recurrences == nil {
		e.Recurrences = map[string]RecurrenceFunc{}
	}
	e.Recurrences[kind] = fn
	e.Manifest.Provides.Recurrences = append(e.Manifest.Provides.Recurrences, kind)
	return e
}

// Family declares event families the extension emits or derives (an
// ingress type's families come with Ingress).
func (e *Extension) Family(families ...protocol.Family) *Extension {
	e.Manifest.Provides.Families = append(e.Manifest.Provides.Families, families...)
	return e
}

// OperationGate registers the extension's gate on other extensions'
// operations and declares it (see extension.Gate). The host asks it only
// when the manifest also declares the operation.gate permission
// (sdk.Permissions(protocol.PermissionOperationGate)), which the operator
// sees at install.
func (e *Extension) OperationGate(fn extension.Gate) *Extension {
	e.Gate = fn
	e.Manifest.Provides.OperationGate = true
	return e
}

// DependencyFilter registers the dependency filter of the extension's wait
// operations and declares it.
func (e *Extension) DependencyFilter(fn func(filter domain.Payload, event domain.Event) bool) *Extension {
	e.Filter = fn
	e.Manifest.Provides.DependencyFilter = true
	return e
}
