package extproc

import (
	"strconv"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// The describe cache stays bounded however many drafts are validated, and
// keeps the lines of the objects validated lately.
func TestDescribedCacheIsBounded(t *testing.T) {
	var c describedCache
	for i := range 5 * describedCap {
		c.put(strconv.Itoa(i), validated{lines: []protocol.DescribeLine{{Detail: strconv.Itoa(i)}}})
	}
	if n := len(c.newer) + len(c.older); n > 2*describedCap {
		t.Fatalf("%d objects kept, more than %d", n, 2*describedCap)
	}
	last := strconv.Itoa(5*describedCap - 1)
	if v, ok := c.get(last); !ok || v.lines[0].Detail != last {
		t.Fatalf("the latest object's lines: %v %v", v, ok)
	}
	if _, ok := c.get("0"); ok {
		t.Fatal("the oldest object's lines were kept")
	}
	// Validating an object again keeps its lines however many follow.
	c.put("current", validated{lines: []protocol.DescribeLine{{Detail: "now"}}, kinds: []string{"agent.task.x"}})
	for i := range describedCap - 1 {
		c.put("draft"+strconv.Itoa(i), validated{})
	}
	if v, ok := c.get("current"); !ok || len(v.kinds) != 1 {
		t.Fatalf("an object validated within the bound was dropped, or lost its work kinds: %+v %v", v, ok)
	}
}
