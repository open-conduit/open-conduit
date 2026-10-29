package web

import (
	"strings"
	"testing"
)

// Every stylesheet closes each block it opens. One missing brace nests the
// rest of a file inside a rule, and browsers drop it without a word.
func TestStylesheetsBalance(t *testing.T) {
	entries, err := assets.ReadDir("static")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".css") {
			continue
		}
		b, err := assets.ReadFile("static/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		depth := 0
		for i, l := range strings.Split(string(b), "\n") {
			depth += strings.Count(l, "{") - strings.Count(l, "}")
			if depth < 0 {
				t.Fatalf("%s:%d closes a block it never opened", e.Name(), i+1)
			}
		}
		if depth != 0 {
			t.Errorf("%s leaves %d block(s) open", e.Name(), depth)
		}
	}
}
