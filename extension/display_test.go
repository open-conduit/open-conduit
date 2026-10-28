package extension_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func TestValidateDisplay(t *testing.T) {
	ok := func() *domain.WorkDisplay {
		return &domain.WorkDisplay{Title: "Fix the example widget", Ref: "#1", Requester: "example-user", Links: []domain.Link{{Label: "Issue #1", URL: "https://tracker.example/issues/1"}}}
	}
	if err := extension.ValidateDisplay(ok()); err != nil {
		t.Fatal(err)
	}
	if err := extension.ValidateDisplay(nil); err != nil {
		t.Fatalf("no display: %v", err)
	}
	if err := extension.ValidateDisplay(&domain.WorkDisplay{Ref: "ENG-1"}); err != nil {
		t.Fatalf("a ref alone: %v", err)
	}
	for name, spoil := range map[string]func(*domain.WorkDisplay){
		"empty":          func(d *domain.WorkDisplay) { d.Title, d.Ref = "", " " },
		"long title":     func(d *domain.WorkDisplay) { d.Title = strings.Repeat("t", extension.MaxDisplayTitle+1) },
		"long ref":       func(d *domain.WorkDisplay) { d.Ref = strings.Repeat("r", extension.MaxDisplayRef+1) },
		"long requester": func(d *domain.WorkDisplay) { d.Requester = strings.Repeat("q", extension.MaxDisplayRequester+1) },
		"two lines":      func(d *domain.WorkDisplay) { d.Title = "one\ntwo" },
		"control":        func(d *domain.WorkDisplay) { d.Ref = "#1\x07" },
		"not utf-8":      func(d *domain.WorkDisplay) { d.Title = "\xff" },
		"javascript":     func(d *domain.WorkDisplay) { d.Links[0].URL = "javascript:alert(1)" },
		"relative":       func(d *domain.WorkDisplay) { d.Links[0].URL = "/issues/1" },
		"no label":       func(d *domain.WorkDisplay) { d.Links[0].Label = "" },
		"long url": func(d *domain.WorkDisplay) {
			d.Links[0].URL = "https://tracker.example/" + strings.Repeat("u", extension.MaxDisplayURL)
		},
		"too many links": func(d *domain.WorkDisplay) {
			for len(d.Links) <= extension.MaxDisplayLinks {
				d.Links = append(d.Links, d.Links[0])
			}
		},
	} {
		d := ok()
		spoil(d)
		if extension.ValidateDisplay(d) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateWaitDescription(t *testing.T) {
	if err := extension.ValidateWaitDescription("checks on PR #2"); err != nil {
		t.Fatal(err)
	}
	if err := extension.ValidateWaitDescription(""); err != nil {
		t.Fatalf("none: %v", err)
	}
	for _, s := range []string{strings.Repeat("w", extension.MaxWaitDescription+1), "a\nb", "a\x00"} {
		if extension.ValidateWaitDescription(s) == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestClip(t *testing.T) {
	for in, want := range map[string]string{
		"  \n  First line \nsecond": "First line",
		"tab\tbell\x07":             "tab bell",
		"":                          "",
	} {
		if got := extension.Clip(in, 100); got != want {
			t.Errorf("Clip(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 150)
	got := extension.Clip(long, extension.MaxDisplayTitle)
	if len(got) > extension.MaxDisplayTitle || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("clipped to %d bytes: %q", len(got), got)
	}
	if err := extension.ValidateDisplay(&domain.WorkDisplay{Title: got}); err != nil {
		t.Fatalf("a clipped title is refused: %v", err)
	}
}
