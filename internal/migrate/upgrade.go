package migrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// Renames maps an extension entry type to the settings it renamed: old name
// to new name. Each extension publishes its own (posthog.Renamed); the
// caller composes them.
type Renames map[string]map[string]string

// Upgrade rewrites an Open Conduit config's renamed extension settings to
// their new names (`migrate upgrade`). Only the renamed keys change: the
// rest of the file keeps its bytes, order and formatting. An entry that
// already gives the new name keeps both and is reported for the operator to
// merge. Like Convert it writes nothing; the caller validates the result.
func Upgrade(config []byte, renames Renames) (Result, error) {
	var doc struct {
		Scopes []struct {
			ID         string `json:"id"`
			Extensions []struct {
				Name   string                     `json:"name"`
				Type   string                     `json:"type"`
				Config map[string]json.RawMessage `json:"config"`
			} `json:"extensions"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return Result{}, errors.New("open conduit config: not a JSON config document")
	}
	res := Result{Scopes: []string{}, Items: []Item{}, Secrets: []Secret{}}
	// rename is the old key to rewrite in each entry's config, by pointer.
	rename := map[string]string{}
	for i, sc := range doc.Scopes {
		res.Scopes = append(res.Scopes, sc.ID)
		for j, e := range sc.Extensions {
			names := renames[e.Type]
			olds := make([]string, 0, len(names))
			for old := range names {
				if _, ok := e.Config[old]; ok {
					olds = append(olds, old)
				}
			}
			sort.Strings(olds)
			for _, old := range olds {
				next := names[old]
				where := fmt.Sprintf("scope %s, %s entry %s: %s", sc.ID, e.Type, e.Name, old)
				target := fmt.Sprintf("/scopes/%d/extensions/%d/config/%s", i, j, next)
				if _, ok := e.Config[next]; ok {
					res.Items = append(res.Items, Item{Level: Action, Path: where, Target: target, Message: "both " + old + " and its new name " + next + " are set: keep one, as " + next})
					continue
				}
				rename[fmt.Sprintf("/scopes/%d/extensions/%d/config/%s", i, j, old)] = next
				res.Items = append(res.Items, Item{Level: Changed, Path: where, Target: target, Message: "renamed to " + next})
			}
		}
	}
	out, err := renameKeys(config, rename)
	if err != nil {
		return Result{}, err
	}
	res.Config = out
	return res, nil
}

// renameKeys rewrites the object keys at the given JSON pointers in place.
func renameKeys(data []byte, rename map[string]string) ([]byte, error) {
	if len(rename) == 0 {
		return data, nil
	}
	type frame struct {
		ptr    string
		object bool
		key    bool // the next string token is a key
		index  int
	}
	type edit struct {
		start, end int
		with       string
	}
	var (
		stack []*frame
		edits []edit
	)
	// child is the pointer of the value about to be read in the top frame.
	child := func(f *frame, key string) string {
		if f.object {
			return f.ptr + "/" + key
		}
		return f.ptr + "/" + strconv.Itoa(f.index)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	lastKey := ""
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("open conduit config: not a JSON config document")
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if s, ok := tok.(string); ok && top != nil && top.object && top.key {
			top.key, lastKey = false, s
			if next, ok := rename[child(top, s)]; ok {
				end := int(d.InputOffset())
				lit, _ := json.Marshal(s)
				start := end - len(lit)
				if start < 0 || !bytes.Equal(data[start:end], lit) {
					return nil, fmt.Errorf("open conduit config: cannot rename %s (escaped key)", s)
				}
				newLit, _ := json.Marshal(next)
				edits = append(edits, edit{start, end, string(newLit)})
			}
			continue
		}
		ptr := ""
		if top != nil {
			ptr = child(top, lastKey)
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			stack = append(stack, &frame{ptr: ptr, object: tok == json.Delim('{'), key: tok == json.Delim('{')})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		// A value ended: the parent moves on to its next key or item.
		if len(stack) > 0 {
			p := stack[len(stack)-1]
			if p.object {
				p.key = true
			} else {
				p.index++
			}
		}
	}
	out := append([]byte(nil), data...)
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		out = append(out[:e.start:e.start], append([]byte(e.with), out[e.end:]...)...)
	}
	return out, nil
}
