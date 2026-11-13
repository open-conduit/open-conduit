package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Config diagnostics (issue #576). An owner that refuses a config object
// it owns (an extension entry's, a connection's, a route handler's, an
// executor binding's or a resource provider's config) may say what is
// wrong with it by returning a *ConfigError. The host passes each
// diagnostic through to `config validate`, `config check` and the
// management API, at the field's full path in the document, with the
// owner's code and message, the way it reports its own checks. A refusal
// that is not a *ConfigError is still reported only as invalid_config at
// the object, since its text could quote configured values.

// Limits on what the host passes through. A diagnostic outside them is
// dropped; the owner's other diagnostics still pass.
const (
	// MaxConfigDiagnostics is how many diagnostics of one refusal pass.
	MaxConfigDiagnostics = 32
	// MaxConfigMessage bounds a diagnostic's message, in bytes.
	MaxConfigMessage = 500
	// MaxConfigPath bounds a diagnostic's path, in bytes.
	MaxConfigPath = 512
)

// ConfigDiagnostic is one thing wrong with a config object.
type ConfigDiagnostic struct {
	// Path is the RFC 6901 JSON pointer of the field within the config
	// object ("/jobs/1/cron"), "" for the object itself. ConfigPointer
	// builds one.
	Path string `json:"path"`
	// Code is stable and snake_case ([a-z][a-z0-9_]{0,63}), such as
	// invalid_cron or unknown_field, so a program can act on it.
	Code string `json:"code"`
	// Message says what is wrong and what is allowed. It is shown to the
	// operator and returned by the management API, so it must be fixed
	// text that never quotes a configured value: a value could be a
	// secret, or a reference to one.
	Message string `json:"message"`
}

// ConfigError is an owner's refusal of a config object with the
// diagnostics that say why. It is extension.ErrInvalid.
type ConfigError struct{ Diagnostics []ConfigDiagnostic }

// Invalid is a refusal with one diagnostic.
func Invalid(path, code, message string) *ConfigError {
	return &ConfigError{Diagnostics: []ConfigDiagnostic{{Path: path, Code: code, Message: message}}}
}

// Add appends a diagnostic.
func (e *ConfigError) Add(path, code, message string) {
	e.Diagnostics = append(e.Diagnostics, ConfigDiagnostic{Path: path, Code: code, Message: message})
}

// Merge appends other's diagnostics, each under prefix (a JSON pointer).
func (e *ConfigError) Merge(prefix string, other *ConfigError) {
	if other == nil {
		return
	}
	for _, d := range other.Diagnostics {
		d.Path = prefix + d.Path
		e.Diagnostics = append(e.Diagnostics, d)
	}
}

// Err is e when it holds a diagnostic, else nil, so a validator can
// collect into a ConfigError and return Err().
func (e *ConfigError) Err() error {
	if e == nil || len(e.Diagnostics) == 0 {
		return nil
	}
	return e
}

func (e *ConfigError) Error() string {
	parts := make([]string, len(e.Diagnostics))
	for i, d := range e.Diagnostics {
		path := d.Path
		if path == "" {
			path = "config"
		}
		parts[i] = path + ": " + d.Code + ": " + d.Message
	}
	return "invalid config: " + strings.Join(parts, "; ")
}

// Unwrap makes a ConfigError extension.ErrInvalid.
func (e *ConfigError) Unwrap() error { return ErrInvalid }

var configCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ConfigDiagnostics are the diagnostics err carries (a *ConfigError
// anywhere in its chain) that are within the limits: a snake_case code, a
// well-formed path and a nonempty printable message, at most
// MaxConfigDiagnostics of them. It is nil when err carries none, and the
// host then reports the refusal without a reason.
func ConfigDiagnostics(err error) []ConfigDiagnostic {
	var ce *ConfigError
	if !errors.As(err, &ce) {
		return nil
	}
	var out []ConfigDiagnostic
	for _, d := range ce.Diagnostics {
		if len(out) == MaxConfigDiagnostics {
			break
		}
		if !configCode.MatchString(d.Code) || !validPointer(d.Path) || !printable(d.Message) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func validPointer(p string) bool {
	if p == "" {
		return true
	}
	if len(p) > MaxConfigPath || p[0] != '/' || !utf8.ValidString(p) {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '~' && (i+1 == len(p) || p[i+1] != '0' && p[i+1] != '1') {
			return false
		}
		if p[i] < 0x20 || p[i] == 0x7f {
			return false
		}
	}
	return true
}

func printable(s string) bool {
	if s == "" || len(s) > MaxConfigMessage || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// ConfigPointer is the JSON pointer of the field a path of object keys
// (strings) and array indexes (ints) names: ConfigPointer("jobs", 1,
// "cron") is "/jobs/1/cron".
func ConfigPointer(parts ...any) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteByte('/')
		switch v := p.(type) {
		case int:
			b.WriteString(strconv.Itoa(v))
		case string:
			b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(v))
		default:
			b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(fmt.Sprint(v)))
		}
	}
	return b.String()
}

// DecodeConfig strictly decodes a config object into out, a pointer to a
// struct, as an owner's validation does: empty data or null decodes as {}.
// It refuses with a *ConfigError whose diagnostics name each field out
// has no place for (unknown_field, listing the allowed ones), each value
// of the wrong JSON type (invalid_type), and malformed JSON or data after
// the object (malformed_config). Fields are matched to json names as
// encoding/json matches them; a field with its own JSON decoding takes
// any value here, and its decoding's refusal is invalid_value at the
// object.
func DecodeConfig(data []byte, out any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		trimmed = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(trimmed))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return Invalid("", "malformed_config", "config must be one JSON object")
	}
	if _, err := d.Token(); err != io.EOF {
		return Invalid("", "malformed_config", "config must be one JSON object, with nothing after it")
	}
	t := reflect.TypeOf(out)
	if t == nil || t.Kind() != reflect.Pointer {
		panic("extension: DecodeConfig needs a pointer")
	}
	ce := &ConfigError{}
	walkConfig(ce, "", v, t.Elem())
	if err := ce.Err(); err != nil {
		return err
	}
	strict := json.NewDecoder(bytes.NewReader(trimmed))
	strict.DisallowUnknownFields()
	if err := strict.Decode(out); err != nil {
		return Invalid("", "invalid_value", "config has a value its fields cannot take; check each against the documented config")
	}
	return nil
}

// walkConfig records what about v a field of type t cannot take, at path.
func walkConfig(ce *ConfigError, path string, v any, t reflect.Type) {
	if v == nil {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawType || t.Kind() == reflect.Interface || reflect.PointerTo(t).Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler) {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			ce.Add(path, "invalid_type", "must be an object")
			return
		}
		fields := configFields(t)
		names := make([]string, 0, len(fields))
		for _, f := range fields {
			names = append(names, f.name)
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			f, ok := findField(fields, k)
			p := path + ConfigPointer(k)
			if !ok {
				ce.Add(p, "unknown_field", "unknown field; allowed fields are "+strings.Join(names, ", "))
				continue
			}
			walkConfig(ce, p, obj[k], f.typ)
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			ce.Add(path, "invalid_type", "must be an object")
			return
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			walkConfig(ce, path+ConfigPointer(k), obj[k], t.Elem())
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			if _, ok := v.(string); !ok {
				ce.Add(path, "invalid_type", "must be a base64 string")
			}
			return
		}
		arr, ok := v.([]any)
		if !ok {
			ce.Add(path, "invalid_type", "must be an array")
			return
		}
		for i, e := range arr {
			walkConfig(ce, path+ConfigPointer(i), e, t.Elem())
		}
	case reflect.String:
		if _, ok := v.(string); !ok {
			ce.Add(path, "invalid_type", "must be a string")
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			ce.Add(path, "invalid_type", "must be true or false")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, ok := v.(json.Number)
		if !ok {
			ce.Add(path, "invalid_type", "must be a whole number")
			return
		}
		if _, err := strconv.ParseInt(string(n), 10, 64); err != nil {
			ce.Add(path, "invalid_type", "must be a whole number")
		}
	case reflect.Float32, reflect.Float64:
		if _, ok := v.(json.Number); !ok {
			ce.Add(path, "invalid_type", "must be a number")
		}
	}
}

type configField struct {
	name string
	typ  reflect.Type
}

// configFields are a struct's fields by json name, embedded untagged
// structs flattened, as encoding/json decodes them.
func configFields(t reflect.Type) []configField {
	var out []configField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				out = append(out, configFields(et)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, configField{name, f.Type})
	}
	return out
}

func findField(fields []configField, key string) (configField, bool) {
	for _, f := range fields {
		if f.name == key {
			return f, true
		}
	}
	for _, f := range fields {
		if strings.EqualFold(f.name, key) {
			return f, true
		}
	}
	return configField{}, false
}
