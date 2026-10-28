package extension

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
)

// Work display and Wait descriptions (issue #123). A source says what a
// Work is (RouteDecision.Display) and what a Wait is for
// (WaitRequest.Description) so operator views can show both without
// reading correlation keys or event types. Core stores the text and never
// interprets it. Text over the limits is refused, never truncated: an
// extension clips its own text with Clip.

// Display and description limits, in bytes.
const (
	MaxDisplayTitle     = 200
	MaxDisplayRef       = 40
	MaxDisplayRequester = 100
	MaxDisplayLinks     = 8
	MaxDisplayLinkLabel = 60
	MaxDisplayURL       = 2048
	MaxWaitDescription  = 200
)

// ValidateDisplay reports why a Work display cannot be stored, or nil. A
// nil display is valid (the Work has none). Every field is one line; a
// display needs a title or a ref; links are labelled http or https URLs.
func ValidateDisplay(d *domain.WorkDisplay) error {
	if d == nil {
		return nil
	}
	if strings.TrimSpace(d.Title) == "" && strings.TrimSpace(d.Ref) == "" {
		return fmt.Errorf("display needs a title or a ref")
	}
	if err := line("display title", d.Title, MaxDisplayTitle, false); err != nil {
		return err
	}
	if err := line("display ref", d.Ref, MaxDisplayRef, false); err != nil {
		return err
	}
	if err := line("display requester", d.Requester, MaxDisplayRequester, false); err != nil {
		return err
	}
	if len(d.Links) > MaxDisplayLinks {
		return fmt.Errorf("display has more than %d links", MaxDisplayLinks)
	}
	for _, l := range d.Links {
		if err := line("display link label", l.Label, MaxDisplayLinkLabel, true); err != nil {
			return err
		}
		if !httpURL(l.URL, MaxDisplayURL) {
			return fmt.Errorf("display link %q is not an http or https URL", l.URL)
		}
	}
	return nil
}

// ValidateWaitDescription reports why a Wait description cannot be stored,
// or nil. Empty is valid (the Wait has none).
func ValidateWaitDescription(s string) error {
	return line("wait description", s, MaxWaitDescription, false)
}

// line checks one single-line string: text without newlines or tabs.
func line(what, s string, limit int, required bool) error {
	if err := text(what, s, limit, required); err != nil {
		return err
	}
	if strings.ContainsAny(s, "\n\t") {
		return fmt.Errorf("%s is not one line", what)
	}
	return nil
}

// Clip makes s fit a one-line display field of limit bytes: its first
// non-empty line, trimmed, cut at a rune boundary with an ellipsis when
// longer. Control characters are dropped.
func Clip(s string, limit int) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			s = l
			break
		}
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…"
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut <= 0 {
		return ""
	}
	return strings.TrimSpace(s[:cut]) + ellipsis
}
