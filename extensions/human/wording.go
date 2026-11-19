package human

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/extension"
)

// Wording (issue #762). A request reads to the person asked in their words,
// in parts a surface lays out itself (extension.Wording): for an approval,
// what the asker wants to do, the call's top-level input as named values,
// and the approve and refuse choices with their tones; for a question, the
// question. What an approval says the call does comes from the gate's
// label in config, else the operation's own sentence (Operation.Wants),
// else the operation's name; a gate's fields name the input's values, and
// its confirm marks the choice a person confirms before it is sent.

// maxFields bounds the values an approval's wording names: past it, the
// input is shown whole.
const maxFields = 10

// maxFieldValue bounds one value as shown.
const maxFieldValue = 500

// fill is template with each {field} replaced by that top-level input
// value, as text; ok is false when a field it names is absent, empty or
// not a single value, so a sentence never reads with a hole in it.
func fill(template string, input json.RawMessage) (string, bool) {
	values := map[string]string{}
	if pairs, ok := topLevel(input); ok {
		for _, p := range pairs {
			if v, ok := scalar(p.value); ok {
				values[p.key] = v
			}
		}
	}
	var b strings.Builder
	rest := template
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			break
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			b.WriteString(rest)
			break
		}
		name := rest[open+1 : open+end]
		v := values[name]
		if strings.TrimSpace(v) == "" {
			return "", false
		}
		b.WriteString(rest[:open])
		b.WriteString(oneLine(v))
		rest = rest[open+end+1:]
	}
	return strings.TrimSpace(b.String()), true
}

// placeholders are the {field} names template uses.
func placeholders(template string) []string {
	var out []string
	rest := template
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return out
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			return out
		}
		out = append(out, rest[open+1:open+end])
		rest = rest[open+end+1:]
	}
}

// scalar is a JSON string, number or boolean as text; false for anything
// else (an object, an array, null).
func scalar(v json.RawMessage) (string, bool) {
	var x any
	d := json.NewDecoder(bytes.NewReader(v))
	d.UseNumber()
	if d.Decode(&x) != nil {
		return "", false
	}
	switch x := x.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		if x {
			return "yes", true
		}
		return "no", true
	}
	return "", false
}

// oneLine drops control characters, so a value can't break the line it
// is shown on.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// clipText cuts s to at most n bytes, on a rune boundary.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// fieldName is how an input field reads when the gate doesn't name it:
// "pull_number" reads "Pull number".
func fieldName(key string) string {
	s := strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(key))
	if s == "" {
		return key
	}
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

// inputWording is an approval's call's input as named values: each
// top-level value (only those the gate's fields list, when it lists any,
// under the names it gives them), and the input as indented JSON when a
// value it shows is nested or there are more than fit: the whole input, or
// with fields listed, only those fields, so a field the gate leaves out is
// never shown.
func inputWording(input json.RawMessage, names map[string]string) ([]extension.WordingField, string) {
	pairs, ok := topLevel(input)
	if !ok {
		if t := strings.TrimSpace(string(input)); t == "" || t == "null" {
			return nil, ""
		}
		return nil, indented(input)
	}
	var fields []extension.WordingField
	whole := false
	var shown []pair
	for _, p := range pairs {
		name := fieldName(p.key)
		if len(names) > 0 {
			given, listed := names[p.key]
			if !listed {
				continue
			}
			if given != "" {
				name = given
			}
		}
		shown = append(shown, p)
		v, ok := scalar(p.value)
		if !ok {
			if t := strings.TrimSpace(string(p.value)); t != "null" {
				whole = true
			}
			continue
		}
		if strings.TrimSpace(v) == "" {
			continue
		}
		fields = append(fields, extension.WordingField{Name: name, Value: clipText(oneLine(v), maxFieldValue)})
	}
	if len(fields) > maxFields {
		fields, whole = fields[:maxFields], true
	}
	switch {
	case !whole:
		return fields, ""
	case len(names) == 0:
		return fields, indented(input)
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range shown {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(p.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(p.value)
	}
	b.WriteByte('}')
	return fields, indented(b.Bytes())
}

// indented is input as indented JSON, clipped.
func indented(input json.RawMessage) string {
	var b bytes.Buffer
	text := string(input)
	if json.Indent(&b, input, "", "  ") == nil {
		text = b.String()
	}
	if len(text) > maxContext {
		cut := maxContext - len("\n…")
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "\n…"
	}
	return text
}

// wants is what a gated call does in a person's words: the gate's label,
// else the operation's own sentence, each with the input's values filled
// in, else that the asker wants to call the operation.
func wants(label, opWants, operation string, input json.RawMessage) string {
	for _, t := range []string{label, opWants} {
		if strings.TrimSpace(t) == "" {
			continue
		}
		if s, ok := fill(t, input); ok {
			return clipText(lowerFirst(s), 300)
		}
	}
	return "call " + operation
}

// approvalChoices are an approval's choices: approve affirms, refuse
// turns it down, and the one the gate's confirm names asks first.
func approvalChoices(confirm, wants string) []extension.ChoiceWording {
	out := []extension.ChoiceWording{
		{Choice: OptionApprove, Label: "Approve", Tone: extension.ToneAffirm},
		{Choice: OptionRefuse, Label: "Refuse", Tone: extension.ToneDanger},
	}
	for i := range out {
		if out[i].Choice != confirm {
			continue
		}
		if confirm == OptionApprove {
			out[i].Confirm = "Approve this? It lets the agent " + wants + "."
		} else {
			out[i].Confirm = "Refuse this? The agent won't " + wants + "."
		}
	}
	return out
}

// wording is rec in a person's words, as of now.
func wording(rec Ask, now time.Time) *extension.Wording {
	w := &extension.Wording{}
	if rec.Gate != "" || rec.Start {
		w.Wants = rec.Wants
		if w.Wants == "" {
			// Asked before wording was recorded.
			w.Wants = "call " + rec.Gate
			if rec.Start {
				w.Wants = "start"
			}
		}
		w.Fields, w.Input = rec.Fields, rec.Input
		w.Choices = approvalChoices(rec.Confirm, w.Wants)
	} else {
		w.Question = rec.Question
		if c := strings.TrimSpace(rec.Context); c != "" {
			w.Note = clipText(c, maxContext)
		}
	}
	if rec.State == StateOpen && rec.escalated(now) && !rec.expired(now) {
		note := "Nobody answered in time, so more people are asked now. Answer by " + rec.Deadline.UTC().Format("2006-01-02 15:04 UTC") + " or it is refused."
		w.Note = strings.TrimSpace(note + "\n\n" + w.Note)
	}
	return w
}

// settledAt is when rec was settled (zero while open, or unknown).
func settledAt(rec Ask) time.Time {
	switch {
	case rec.State == StateAnswered && rec.Answer != nil:
		return rec.Answer.At
	case rec.State == StateExpired && !rec.Deadline.IsZero():
		return rec.Deadline
	}
	return rec.ClosedAt
}
