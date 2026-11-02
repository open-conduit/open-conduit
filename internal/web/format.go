package web

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// dur is a duration as the prototype writes it: "42s", "7m" ("7m 12s"
// precise under ten minutes), "3h 5m", "2d 4h".
func dur(d time.Duration, precise bool) string {
	if d < 0 {
		d = 0
	}
	s := int((d + time.Second/2) / time.Second)
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	m := s / 60
	if m < 60 {
		if precise && m < 10 {
			return fmt.Sprintf("%dm %ds", m, s%60)
		}
		return strconv.Itoa(m) + "m"
	}
	h := m / 60
	if h < 24 {
		if m%60 != 0 {
			return fmt.Sprintf("%dh %dm", h, m%60)
		}
		return strconv.Itoa(h) + "h"
	}
	days := h / 24
	if h%24 != 0 {
		return fmt.Sprintf("%dd %dh", days, h%24)
	}
	return strconv.Itoa(days) + "d"
}

// since is how long ago t was: "just now", "4m ago".
func since(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	if now.Sub(t) < 45*time.Second {
		return "just now"
	}
	return dur(now.Sub(t), false) + " ago"
}

// until is how long until t: "in 5m".
func until(t, now time.Time) string {
	return "in " + dur(t.Sub(now), false)
}

// clockTime is t's local time of day, with "Yesterday" or the date when it
// was not today.
func clockTime(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	t, now = t.Local(), now.Local()
	hm := t.Format("15:04")
	y, m, d := t.Date()
	ny, nm, nd := now.Date()
	if y == ny && m == nm && d == nd {
		return hm
	}
	py, pm, pd := now.AddDate(0, 0, -1).Date()
	if y == py && m == pm && d == pd {
		return "Yesterday " + hm
	}
	return t.Format("2 Jan") + " " + hm
}

// titleOf is a source's display name: the core's own, or the title its
// extension gives (GET /v1/extensions, from its manifest), else its name.
func titleOf(titles map[string]string, s string) string {
	if s == "core" {
		return "Core"
	}
	if t := titles[s]; t != "" {
		return t
	}
	return s
}

// sourceLabel is the source's display name, from the extensions' titles,
// cached briefly; a daemon too old to list them gives none.
func (s *Server) sourceLabel(name string) string {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	now := s.opts.Now()
	if s.tat.IsZero() || now.Sub(s.tat) > worldTTL || now.Before(s.tat) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		s.tcache = map[string]string{}
		if ts, err := s.src.Extensions(ctx); err == nil {
			for _, t := range ts {
				s.tcache[t.Name] = t.Title
			}
		}
		s.tat = now
	}
	return titleOf(s.tcache, name)
}

// money is a cost in dollars.
func money(n float64) string {
	if n < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", n)
}

// kTok is a token count: "812", "14k", "1.2M".
func kTok(n uint64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return strconv.FormatUint((n+500)/1000, 10) + "k"
	}
	return strconv.FormatUint(n, 10)
}
