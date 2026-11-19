package slack

import (
	"reflect"
	"strings"
	"testing"
)

// Every field of a slack entry's config is in the operator's catalogue
// (`open-conduit adapters slack`), so one the config accepts is never
// missing from what the operator is shown (issue #759).
func TestEntryFieldsListEveryConfigField(t *testing.T) {
	listed := map[string]bool{}
	for _, f := range entryFields() {
		listed[f.Name] = true
	}
	typ := reflect.TypeFor[ExtensionConfig]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" && !listed[name] {
			t.Errorf("entry field %q is not listed", name)
		}
	}
}
