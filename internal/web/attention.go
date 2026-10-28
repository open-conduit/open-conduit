package web

import (
	"net/http"
	"time"
)

// attentionData is Needs you: what is blocked on a person.
type attentionData struct {
	// Decisions are open Work an extension offers an action on, whatever
	// the extension.
	Decisions []attentionItem
	// Repairs stopped and need one: retry exhausted or blocked, an
	// operator-cancelled turn.
	Repairs []workView
	Held    []workView
	// Watch is not blocked on anyone but worth a look: long waits, and
	// waiting Work whose latest turn failed.
	Watch    []workView
	Receipts []receiptView
	Page     *page
}

// attentionItem is a decision with its panels that offer actions, drawn
// prominent, and the actions no panel shows.
type attentionItem struct {
	W      workView
	Panels []panelView
	Extra  []actionView
}

// receiptView is a receipt with the Work it was made on, when known.
type receiptView struct {
	receipt
	Href string
}

// watchAfter is how long a Wait goes unchanged before it is worth a look.
const watchAfter = 36 * time.Hour

func attention(works []workView, p *page, now time.Time) attentionData {
	d := attentionData{Page: p}
	for _, w := range works {
		if w.Open() && len(w.Actions) > 0 {
			d.Decisions = append(d.Decisions, attentionItem{W: w, Panels: workPanels(w, p, "acting"), Extra: unshownOn(w, p)})
		}
		if w.Disp.Kind == "repair" {
			d.Repairs = append(d.Repairs, w)
		}
		if w.Hold != nil {
			d.Held = append(d.Held, w)
		}
		if w.Hold == nil && w.Disp.Group == "waiting" {
			last := w.Last()
			if now.Sub(w.Disp.Since) > watchAfter || last != nil && last.State == "failed" {
				d.Watch = append(d.Watch, w)
			}
		}
	}
	return d
}

func (s *Server) attentionPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Needs you", Nav: "attention"}
	wd := s.cachedWorld(ctx, true)
	if len(wd.Errs) > 0 {
		p.Err = wd.Errs[0]
	}
	d := attention(wd.Works, p, s.opts.Now())
	names := map[string]string{}
	for _, x := range wd.Works {
		names[x.ID] = workName(x)
	}
	for _, rc := range s.receipts.list() {
		v := receiptView{receipt: rc}
		if rc.WorkID != "" {
			v.Href = workHref(rc.WorkID, 0)
			if n, ok := names[rc.WorkID]; ok {
				v.Work = n
			}
		}
		d.Receipts = append(d.Receipts, v)
	}
	s.render(w, r, "attention", p, d)
}
