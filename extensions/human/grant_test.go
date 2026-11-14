package human

import (
	"encoding/json"
	"testing"
	"time"
)

// valid_for needs operations and lasts 1m to 24h; of several gates on a
// call, the shortest grant holds, and none if any gate has none.
func TestParseValidFor(t *testing.T) {
	for _, bad := range []string{`{"gates":[{"kinds":["x.*"],"valid_for":"15m"}]}`, `{"gates":[{"operations":["x.y"],"valid_for":"30s"}]}`, `{"gates":[{"operations":["x.y"],"valid_for":"25h"}]}`} {
		if _, err := ParseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	c, err := ParseConfig(json.RawMessage(`{"gates":[{"operations":["x.y"],"valid_for":"1h"},{"operations":["x.*"],"valid_for":"10m"},{"operations":["x.y"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gate(c.gates("x.y", nil, "", false)[:2]).validFor; got != 10*time.Minute {
		t.Errorf("two grants: %v", got)
	}
	if got := gate(c.gates("x.y", nil, "", false)).validFor; got != 0 {
		t.Errorf("a gate with no grant: %v", got)
	}
}
