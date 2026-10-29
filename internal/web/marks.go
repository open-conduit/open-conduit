package web

import (
	"fmt"
	"html/template"
	"strings"
)

// markSVG draws a state as a shape and a colour, never colour alone, as
// the prototype's StateMark does. kind is a disposition kind or a turn
// state; an unknown kind draws nothing. A titled mark is an image with that
// title; an untitled one is hidden from assistive technology.
func markSVG(kind string, size int, title string) template.HTML {
	s := float64(size)
	c := s / 2
	f := func(v float64) string { return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".") }
	var class, body string
	switch kind {
	case "decision":
		class = "mark-signal"
		body = fmt.Sprintf(`<path d="M%s 0.5 L%s %s L%s %s L0.5 %sZ" fill="currentColor"/>`, f(c), f(s-0.5), f(c), f(c), f(s-0.5), f(c))
	case "repair":
		class = "mark-signal"
		body = fmt.Sprintf(`<path d="M%s 1.4 L%s %s L%s %s L1.4 %sZ" fill="none" stroke="currentColor" stroke-width="1.8"/><circle cx="%s" cy="%s" r="1.4" fill="currentColor"/>`, f(c), f(s-1.4), f(c), f(c), f(s-1.4), f(c), f(c), f(c))
	case "running":
		class = "mark-run"
		body = fmt.Sprintf(`<circle cx="%s" cy="%s" r="%s" fill="none" stroke="currentColor" stroke-opacity=".35" class="pulse"/><circle cx="%s" cy="%s" r="%s" fill="currentColor"/>`, f(c), f(c), f(c-0.5), f(c), f(c), f(c*0.55))
	case "pending", "queued":
		class = "mark-muted"
		body = fmt.Sprintf(`<circle cx="%s" cy="%s" r="%s" fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 1.6"/>`, f(c), f(c), f(c-1))
	case "waiting", "parked", "handoff":
		class = "mark-wait"
		body = fmt.Sprintf(`<circle cx="%s" cy="%s" r="%s" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M%s 0.9 A%s %s 0 0 0 %s %sZ" fill="currentColor"/>`, f(c), f(c), f(c-0.9), f(c), f(c-0.9), f(c-0.9), f(c), f(s-0.9))
	case "held":
		class = "mark-held"
		body = fmt.Sprintf(`<rect x="1.5" y="1.5" width="%s" height="%s" fill="currentColor"/><rect x="%s" y="3.5" width="1.4" height="%s" fill="var(--sheet)"/><rect x="%s" y="3.5" width="1.4" height="%s" fill="var(--sheet)"/>`, f(s-3), f(s-3), f(c-2.2), f(s-7), f(c+0.8), f(s-7))
	case "completed":
		class = "mark-done"
		body = fmt.Sprintf(`<path d="M1.5 %s L%s %s L%s 2.2" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="square"/>`, f(c+0.5), f(c-1), f(s-2.5), f(s-1.2))
	case "failed":
		class = "mark-fail"
		body = fmt.Sprintf(`<path d="M2 2 L%s %s M%s 2 L2 %s" stroke="currentColor" stroke-width="1.9"/>`, f(s-2), f(s-2), f(s-2), f(s-2))
	case "cancelled":
		class = "mark-muted"
		body = fmt.Sprintf(`<circle cx="%s" cy="%s" r="%s" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M2.6 %s L%s 2.6" stroke="currentColor" stroke-width="1.4"/>`, f(c), f(c), f(c-1), f(s-2.6), f(s-2.6))
	default:
		return ""
	}
	a11y := ` aria-hidden="true"`
	if title != "" {
		a11y = ` role="img"`
		body = "<title>" + template.HTMLEscapeString(title) + "</title>" + body
	}
	return template.HTML(fmt.Sprintf(`<svg width="%d" height="%d" viewBox="0 0 %d %d"%s class="mark %s">%s</svg>`, size, size, size, size, a11y, class, body))
}

var toneColour = map[string]string{"run": "var(--run)", "fail": "var(--fail)", "muted": "var(--ink-3)", "ink": "var(--ink)"}

// lifelineSVG draws a lifeline's geometry: the dotted lifetime, a tick per
// Event and a bar per active segment, with the disposition mark at its end
// while the Work is open.
func lifelineSVG(l lifeline) template.HTML {
	var b strings.Builder
	h := l.Height + 6
	fmt.Fprintf(&b, `<span class="lifeline"><svg viewBox="0 0 %d %d" preserveAspectRatio="none" width="100%%" height="%d" aria-hidden="true">`, lifeWidth, h, h)
	fmt.Fprintf(&b, `<line x1="4" x2="%.1f" y1="%.1f" y2="%.1f" stroke="var(--rule-2)" stroke-width="1.5" stroke-dasharray="3 5" vector-effect="non-scaling-stroke"/>`, l.EndX, l.Mid, l.Mid)
	for _, t := range l.Ticks {
		c := "var(--rule-2)"
		if t.Person {
			c = "var(--ink-2)"
		}
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="1" y2="%.1f" stroke="%s" stroke-width="1.25" vector-effect="non-scaling-stroke"/>`, t.X, t.X, l.Mid-7, c)
	}
	for _, r := range l.Bars {
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="8" fill="%s"/>`, r.X, l.Mid-4, r.Width, toneColour[r.Tone])
	}
	b.WriteString(`</svg>`)
	if l.Mark != "" {
		b.WriteString(string(markSVG(l.Mark, 9, "")))
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

// ratioSVG is the proportion bar of active execution in elapsed lifetime.
func ratioSVG(pct float64) template.HTML {
	return template.HTML(fmt.Sprintf(`<svg class="ratio" viewBox="0 0 100 3" preserveAspectRatio="none" aria-hidden="true"><rect width="%.1f" height="3"/></svg>`, pct))
}
