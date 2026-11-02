package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type kind uint8

const (
	kNull kind = iota
	kObject
	kArray
	kString
	kNumber
	kBool
)

// node is a strictly decoded JSON value retaining object key order.
type node struct {
	kind  kind
	keys  []string
	obj   map[string]*node
	items []*node
	str   string
	num   json.Number
	b     bool
}

func (n *node) field(k string) *node {
	if n == nil || n.kind != kObject {
		return nil
	}
	return n.obj[k]
}

// value converts to plain Go values; maps marshal with sorted keys.
func (n *node) value() any {
	switch n.kind {
	case kObject:
		m := make(map[string]any, len(n.keys))
		for _, k := range n.keys {
			m[k] = n.obj[k].value()
		}
		return m
	case kArray:
		a := make([]any, len(n.items))
		for i, item := range n.items {
			a[i] = item.value()
		}
		return a
	case kString:
		return n.str
	case kNumber:
		return n.num
	case kBool:
		return n.b
	}
	return nil
}

// canonical marshals with every object's keys sorted, including struct values.
func canonical(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var plain any
		if err = dec.Decode(&plain); err == nil {
			b, err = json.Marshal(plain)
		}
	}
	if err != nil {
		panic(err) // only plain decoded values and tagged structs are marshaled
	}
	return b
}

// ptr appends an escaped JSON pointer token.
func ptr(base string, token any) string {
	s := strings.NewReplacer("~", "~0", "/", "~1").Replace(fmt.Sprint(token))
	if base == "" {
		return s
	}
	return base + "/" + s
}

type decoder struct {
	dec   *json.Decoder
	diags []Diagnostic
}

// decode rejects syntax errors, trailing data, duplicate keys and nulls.
func decode(data []byte) (*node, []Diagnostic) {
	if len(data) > MaxSize {
		return nil, []Diagnostic{{"", "too_large", "configuration exceeds 1 MiB"}}
	}
	d := &decoder{dec: json.NewDecoder(bytes.NewReader(data))}
	d.dec.UseNumber()
	n, err := d.value("")
	if err == nil {
		if _, err = d.dec.Token(); err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		msg := "invalid JSON; fix the document syntax"
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			msg = fmt.Sprintf("invalid JSON near byte %d; fix the document syntax", syntax.Offset)
		}
		return nil, []Diagnostic{{"", "invalid_json", msg}}
	}
	return n, d.diags
}
func (d *decoder) value(path string) (*node, error) {
	t, err := d.dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := t.(type) {
	case json.Delim:
		if v == '{' {
			n := &node{kind: kObject, obj: map[string]*node{}}
			for d.dec.More() {
				t, err := d.dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := t.(string)
				child, err := d.value(ptr(path, k))
				if err != nil {
					return nil, err
				}
				if _, dup := n.obj[k]; dup {
					d.diags = append(d.diags, Diagnostic{ptr(path, k), "duplicate_key", "duplicate object key; keep exactly one"})
					continue
				}
				n.keys, n.obj[k] = append(n.keys, k), child
			}
			_, err := d.dec.Token()
			return n, err
		}
		n := &node{kind: kArray}
		for d.dec.More() {
			child, err := d.value(ptr(path, len(n.items)))
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, child)
		}
		_, err := d.dec.Token()
		return n, err
	case string:
		return &node{kind: kString, str: v}, nil
	case json.Number:
		return &node{kind: kNumber, num: v}, nil
	case bool:
		return &node{kind: kBool, b: v}, nil
	}
	d.diags = append(d.diags, Diagnostic{path, "null_value", "explicit null is not allowed; omit the field"})
	return &node{kind: kNull}, nil
}

// checker reports schema-class diagnostics. Null nodes were already reported.
// file checks a config file as written (includes, no scope sources);
// fragment checks an included folder's scopes (no secret references).
type checker struct {
	diags          []Diagnostic
	file, fragment bool
}

func (c *checker) fail(path, code, msg string) {
	c.diags = append(c.diags, Diagnostic{path, code, msg})
}
func (c *checker) typed(n *node, path string, k kind, name string) bool {
	if n.kind == kNull {
		return false
	}
	if n.kind != k {
		c.fail(path, "invalid_type", "expected "+name)
		return false
	}
	return true
}
func (c *checker) object(n *node, path string, required []string, optional ...string) bool {
	if !c.typed(n, path, kObject, "object") {
		return false
	}
	for _, k := range n.keys {
		if !slices.Contains(required, k) && !slices.Contains(optional, k) {
			c.fail(ptr(path, k), "unknown_field", "unknown field; remove it")
		}
	}
	for _, k := range required {
		if n.obj[k] == nil {
			c.fail(ptr(path, k), "missing_field", "required field is missing")
		}
	}
	return true
}
func (c *checker) str(n *node, path string, pattern *regexp.Regexp) (string, bool) {
	if n == nil || !c.typed(n, path, kString, "string") {
		return "", false
	}
	if pattern != nil && !pattern.MatchString(n.str) {
		c.fail(path, "invalid_value", "value does not match the required pattern")
		return "", false
	}
	return n.str, true
}
func (c *checker) enum(n *node, path string, values ...string) (string, bool) {
	s, ok := c.str(n, path, nil)
	if ok && !slices.Contains(values, s) {
		c.fail(path, "invalid_value", "value must be one of "+strings.Join(values, ", "))
		return "", false
	}
	return s, ok
}
func (c *checker) integer(n *node, path string) (int, bool) {
	if n == nil || !c.typed(n, path, kNumber, "integer") {
		return 0, false
	}
	v, err := strconv.ParseInt(n.num.String(), 10, 64)
	if err != nil {
		c.fail(path, "invalid_type", "expected integer")
		return 0, false
	}
	if v < 1 || v > math.MaxInt32 {
		c.fail(path, "invalid_value", "integer must be between 1 and 2147483647")
		return 0, false
	}
	return int(v), true
}

// number is a finite JSON number.
func (c *checker) number(n *node, path string) (float64, bool) {
	if n == nil || !c.typed(n, path, kNumber, "number") {
		return 0, false
	}
	v, err := n.num.Float64()
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		c.fail(path, "invalid_type", "expected a finite number")
		return 0, false
	}
	return v, true
}
func (c *checker) boolean(n *node, path string) (bool, bool) {
	if n == nil || !c.typed(n, path, kBool, "boolean") {
		return false, false
	}
	return n.b, true
}
func (c *checker) list(n *node, path string, pattern *regexp.Regexp) []string {
	out := []string{}
	if n == nil || !c.typed(n, path, kArray, "array") {
		return out
	}
	for i, item := range n.items {
		if s, ok := c.str(item, ptr(path, i), pattern); ok {
			if slices.Contains(out, s) {
				c.fail(ptr(path, i), "duplicate_value", "array items must be unique")
				continue
			}
			out = append(out, s)
		}
	}
	return out
}
func (c *checker) opaque(n *node, path string) json.RawMessage {
	if n == nil || !c.typed(n, path, kObject, "object") {
		return json.RawMessage("{}")
	}
	return canonical(n.value())
}
