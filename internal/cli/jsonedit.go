package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// node is an order-preserving JSON value, so local config edits keep the
// owner's key order and leave everything they did not touch as written.
type node struct {
	obj    []member
	arr    []*node
	scalar json.RawMessage
	kind   byte // 'o', 'a' or 's'
}

type member struct {
	key   string
	value *node
}

func parseNode(data []byte) (*node, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	n, err := readNode(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return n, nil
}

func readNode(d *json.Decoder) (*node, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &node{kind: 'o'}
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				key, _ := k.(string)
				if seen[key] {
					return nil, fmt.Errorf("duplicate key %q", key)
				}
				seen[key] = true
				v, err := readNode(d)
				if err != nil {
					return nil, err
				}
				n.obj = append(n.obj, member{key, v})
			}
			_, err = d.Token()
			return n, err
		case '[':
			n := &node{kind: 'a'}
			for d.More() {
				v, err := readNode(d)
				if err != nil {
					return nil, err
				}
				n.arr = append(n.arr, v)
			}
			_, err = d.Token()
			return n, err
		}
	}
	raw, err := marshal(tok)
	return &node{kind: 's', scalar: raw}, err
}

// marshal encodes without HTML escaping, so text round-trips as written.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func (n *node) write(b *bytes.Buffer, indent string) {
	switch n.kind {
	case 'o':
		if len(n.obj) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, m := range n.obj {
			key, _ := marshal(m.key)
			b.WriteString(indent + "  " + string(key) + ": ")
			m.value.write(b, indent+"  ")
			if i < len(n.obj)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case 'a':
		if len(n.arr) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, v := range n.arr {
			b.WriteString(indent + "  ")
			v.write(b, indent+"  ")
			if i < len(n.arr)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	default:
		b.Write(n.scalar)
	}
}

func (n *node) bytes() []byte {
	var b bytes.Buffer
	n.write(&b, "")
	b.WriteByte('\n')
	return b.Bytes()
}

func (n *node) field(key string) *node {
	for _, m := range n.obj {
		if m.key == key {
			return m.value
		}
	}
	return nil
}

// pointer splits an RFC 6901 pointer ("" is the whole document).
func pointer(p string) ([]string, error) {
	if p == "" || p == "/" {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, errors.New("a pointer starts with / (for example /settings/concurrency)")
	}
	parts := strings.Split(p[1:], "/")
	for i, s := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

// index resolves an array segment: a number, or the id/name of an object
// element (so /scopes/web/bindings/agent addresses by name).
func (n *node) index(seg string) (int, bool) {
	if i, err := strconv.Atoi(seg); err == nil {
		return i, i >= 0 && i < len(n.arr)
	}
	for i, el := range n.arr {
		if el.kind != 'o' {
			continue
		}
		for _, k := range []string{"id", "name"} {
			if f := el.field(k); f != nil && f.kind == 's' && string(f.scalar) == strconv.Quote(seg) {
				return i, true
			}
		}
	}
	return 0, false
}

func (n *node) lookup(parts []string) (*node, error) {
	cur := n
	for _, seg := range parts {
		switch cur.kind {
		case 'o':
			next := cur.field(seg)
			if next == nil {
				return nil, fmt.Errorf("no %q here", seg)
			}
			cur = next
		case 'a':
			i, ok := cur.index(seg)
			if !ok {
				return nil, fmt.Errorf("no element %q", seg)
			}
			cur = cur.arr[i]
		default:
			return nil, fmt.Errorf("%q is inside a scalar", seg)
		}
	}
	return cur, nil
}

// set replaces or adds the value at parts; "-" appends to an array.
func (n *node) set(parts []string, value *node) error {
	if len(parts) == 0 {
		return errors.New("refusing to replace the whole config; set a field")
	}
	parent, err := n.lookup(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	last := parts[len(parts)-1]
	switch parent.kind {
	case 'o':
		for i := range parent.obj {
			if parent.obj[i].key == last {
				parent.obj[i].value = value
				return nil
			}
		}
		parent.obj = append(parent.obj, member{last, value})
		return nil
	case 'a':
		if last == "-" {
			parent.arr = append(parent.arr, value)
			return nil
		}
		i, ok := parent.index(last)
		if !ok {
			return fmt.Errorf("no element %q (use - to append)", last)
		}
		parent.arr[i] = value
		return nil
	}
	return errors.New("cannot set inside a scalar")
}

func (n *node) unset(parts []string) error {
	if len(parts) == 0 {
		return errors.New("refusing to remove the whole config")
	}
	parent, err := n.lookup(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	last := parts[len(parts)-1]
	switch parent.kind {
	case 'o':
		for i := range parent.obj {
			if parent.obj[i].key == last {
				parent.obj = append(parent.obj[:i], parent.obj[i+1:]...)
				return nil
			}
		}
	case 'a':
		if i, ok := parent.index(last); ok {
			parent.arr = append(parent.arr[:i], parent.arr[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("no %q to remove", last)
}

// redact replaces every secret_refs value: reference text is sensitive in
// views even though it is not the secret itself.
func (n *node) redact() {
	switch n.kind {
	case 'o':
		for _, m := range n.obj {
			if m.key == "secret_refs" && m.value.kind == 'o' {
				for i := range m.value.obj {
					m.value.obj[i].value = &node{kind: 's', scalar: json.RawMessage(`"[redacted]"`)}
				}
				continue
			}
			m.value.redact()
		}
	case 'a':
		for _, v := range n.arr {
			v.redact()
		}
	}
}
