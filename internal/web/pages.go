package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// LogTail bounds how much of an output the view fetches.
const LogTail = 64 << 10

// page is what every page template gets: the shell's status line and
// counts as well as the page's own title and errors.
type page struct {
	Title    string
	Nav      string
	CSRF     string
	Back     string
	Flash    *flashNote
	FlashErr bool
	Err      string
	// Down says the daemon did not answer: the page shows the steps to
	// bring it back instead of its content.
	Down     bool
	Health   *client.Health
	Pools    []client.CapacityPool
	NeedsYou int
}

// pageNames are the page templates; each is parsed with the shared ones.
var pageNames = []string{"runs", "work", "attention", "config", "setup", "output", "confirm"}

// shared are the templates every page can use.
var shared = []string{"templates/layout.html", "templates/panels.html", "templates/conversation.html", "templates/actions.html"}

func (s *Server) parse() error {
	now := func() time.Time { return s.opts.Now() }
	funcs := template.FuncMap{
		"trigger":  runview.TriggerText,
		"outcome":  runview.Outcome,
		"time":     runview.ShortTime,
		"duration": func(r client.Run) string { return runview.Duration(r, now()) },
		"live":     runview.Running,
		"ago":      func(ts string) string { return ago(ts, now()) },
		"plural":   plural,
		"summary":  summary,
		"inc":      func(i int) int { return i + 1 },
		"markdown": markdown,
		"clock":    clock,
		"size":     size,
		"mark":     markSVG,
		"lifeline": lifelineSVG,
		"ratio":    ratioSVG,
		"dur":      func(d time.Duration) string { return dur(d, false) },
		"durp":     func(d time.Duration) string { return dur(d, true) },
		"since":    func(t time.Time) string { return since(t, now()) },
		"until":    func(t time.Time) string { return until(t, now()) },
		"at":       func(t time.Time) string { return clockTime(t, now()) },
		"poolName": poolName,
		"source":   sourceLabel,
		"now":      now,
	}
	for k, f := range workFuncs {
		funcs[k] = f
	}
	s.pages = map[string]*template.Template{}
	for _, name := range pageNames {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, append(shared, "templates/"+name+".html")...)
		if err != nil {
			return err
		}
		s.pages[name] = t
	}
	return nil
}

// render draws a page in the shell. The status line reads the daemon's
// health and slots; when the daemon does not answer, the page is the
// unreachable page with status 502.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *page, data any) {
	p.CSRF, p.Back = s.csrf, r.URL.RequestURI()
	p.Flash, p.FlashErr = takeFlash(w, r)
	s.shell(r.Context(), p)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if p.Down {
		w.WriteHeader(http.StatusBadGateway)
	}
	if err := s.pages[name].Execute(w, map[string]any{"Page": p, "Data": data}); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// shell fills in the status line and the Needs you count.
func (s *Server) shell(ctx context.Context, p *page) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	h, err := s.src.Health(ctx)
	if err != nil {
		p.Down = true
		if p.Err == "" {
			p.Err = errText(err)
		}
		return
	}
	p.Health = &h
	if c, err := s.src.Capacity(ctx); err == nil {
		p.Pools = c.Pools
	}
	if p.NeedsYou == 0 {
		p.NeedsYou = s.needsYou(ctx)
	}
}

// poolName is a capacity pool's name on the status line.
func poolName(name string) string {
	if name == "agents" {
		return "agent"
	}
	return strings.TrimSuffix(name, "s")
}

func call(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), callTimeout)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func actionsErr(err error) string {
	if tooOld(err) {
		return "this daemon does not offer actions; upgrade it to answer and claim from here"
	}
	return errText(err)
}

// tooOld reports a daemon without the route asked for.
func tooOld(err error) bool {
	var e *client.Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == "not_found"
}

func statusOf(err error) int {
	var e *client.Error
	if errors.As(err, &e) && e.Status >= 400 {
		return e.Status
	}
	return http.StatusBadGateway
}

func back(r *http.Request) string {
	b := r.PostFormValue("back")
	if !strings.HasPrefix(b, "/") || strings.HasPrefix(b, "//") || strings.HasPrefix(b, "/\\") {
		return "/"
	}
	return b
}

func (s *Server) done(w http.ResponseWriter, r *http.Request, text string, err error) {
	if err != nil {
		flash(w, errText(err), true)
	} else {
		flash(w, text, false)
	}
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

const reason = "operator (web)"

// workControl runs one control against the Work's current revision.
