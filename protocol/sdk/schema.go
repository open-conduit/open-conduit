package sdk

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// SchemaOf returns the JSON Schema of t's encoding/json form, as an
// operation's input declares it. A struct is an object of its exported
// fields under their json names, embedded structs flattened; a field is
// required unless its tag says omitempty or omitzero or it is a pointer;
// and an object refuses fields it does not have, as the operation's
// decoding does. A field's `doc` tag becomes its description. A type with
// its own JSON encoding (json.Marshaler, encoding.TextMarshaler) is any
// value, except time.Time, which is a date-time string.
func SchemaOf(t reflect.Type) map[string]any {
	return schemaOf(t, map[reflect.Type]bool{})
}

var (
	timeType          = reflect.TypeFor[time.Time]()
	rawType           = reflect.TypeFor[json.RawMessage]()
	jsonMarshaler     = reflect.TypeFor[json.Marshaler]()
	textMarshaler     = reflect.TypeFor[encoding.TextMarshaler]()
	jsonUnmarshaler   = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler   = reflect.TypeFor[encoding.TextUnmarshaler]()
	customJSONEncoded = []reflect.Type{jsonMarshaler, textMarshaler, jsonUnmarshaler, textUnmarshaler}
)

func schemaOf(t reflect.Type, visiting map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawType:
		return map[string]any{}
	}
	for _, i := range customJSONEncoded {
		if t.Implements(i) || reflect.PointerTo(t).Implements(i) {
			return map[string]any{}
		}
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 && t.Kind() == reflect.Slice {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": schemaOf(t.Elem(), visiting)}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return map[string]any{"type": "object"}
		}
		return map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem(), visiting)}
	case reflect.Struct:
		if visiting[t] {
			return map[string]any{"type": "object"} // a recursive type: its shape is not repeated
		}
		visiting[t] = true
		defer delete(visiting, t)
		props, required := map[string]any{}, []string{}
		fields(t, visiting, props, &required)
		out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	return map[string]any{}
}

func fields(t reflect.Type, visiting map[reflect.Type]bool, props map[string]any, required *[]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				fields(ft, visiting, props, required)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		s := schemaOf(f.Type, visiting)
		if doc := f.Tag.Get("doc"); doc != "" {
			s["description"] = doc
		}
		props[name] = s
		optional := f.Type.Kind() == reflect.Pointer
		for _, o := range strings.Split(opts, ",") {
			if o == "omitempty" || o == "omitzero" {
				optional = true
			}
		}
		if !optional {
			*required = append(*required, name)
		}
	}
}
