package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
)

// The columns each list prints by default: JSON field names, dotted for a
// nested field. --json prints the records instead.
var (
	scopeColumns     = []string{"id", "name", "parent_id", "enabled", "revision"}
	workColumns      = []string{"id", "state", "kind", "scope_id", "display.title", "updated_at"}
	eventColumns     = []string{"sequence", "id", "type", "scope_id", "work_id", "occurred_at"}
	executionColumns = []string{"id", "state", "work_id", "executor_binding", "attempt", "started_at"}
	waitColumns      = []string{"id", "state", "work_id", "execution_id", "description", "due_at"}
	artifactColumns  = []string{"id", "media_type", "size", "work_id", "execution_id"}
	actionColumns    = []string{"provider", "id", "label"}
)

// table prints v (a page with items and next_cursor, or a plain list) as
// aligned columns, with the next cursor after the rows when there is one.
func (o *operator) table(v any, columns []string) int {
	b, err := json.Marshal(v)
	if err != nil {
		return o.emit(nil, err)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	if err = json.Unmarshal(b, &page.Items); err != nil {
		if err = json.Unmarshal(b, &page); err != nil {
			return o.emit(nil, err)
		}
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(o.out, "none")
		return 0
	}
	w := tabwriter.NewWriter(o.out, 0, 2, 2, ' ', 0)
	head := make([]string, len(columns))
	for i, c := range columns {
		head[i] = strings.ToUpper(strings.ReplaceAll(c[strings.LastIndex(c, ".")+1:], "_", " "))
	}
	fmt.Fprintln(w, strings.Join(head, "\t"))
	for _, item := range page.Items {
		row := make([]string, len(columns))
		for i, c := range columns {
			row[i] = cell(item, c)
		}
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	_ = w.Flush()
	if page.NextCursor != "" {
		fmt.Fprintf(o.out, "next cursor: %s\n", page.NextCursor)
	}
	return 0
}

// cell is the field at a dotted path in item, as one line of text ("-"
// when absent).
func cell(item map[string]any, path string) string {
	var v any = item
	for _, part := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return "-"
		}
		v = m[part]
	}
	var s string
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		s = x
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		s = fmt.Sprint(x)
	default:
		b, _ := json.Marshal(x)
		s = string(b)
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "-"
	}
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}
