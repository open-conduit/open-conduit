package web

import (
	"context"
	"net/http"
	"time"

	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
)

// Updates checks for releases (an *upgrade.Updater). Installing one is
// the daemon's (Source.InstallRelease), which checks the admin role and
// records it in the operator trail.
type Updates interface {
	// Cached is the last release found, never waiting on the network.
	Cached() (upgrade.Release, bool)
	// Latest asks for the latest release now.
	Latest(ctx context.Context) (upgrade.Release, error)
	// Homebrew says whether brew owns the install.
	Homebrew() bool
}

// Service is the background service the daemon may run under (a
// *cli.ServiceControl).
type Service interface {
	Running() bool
	// Restart restarts it on the installed binary; it may stop this process.
	Restart() error
}

// installTimeout bounds the daemon's downloading and installing a release.
const installTimeout = 5 * time.Minute

// updateView is the update page.
type updateView struct {
	Running    string
	Latest     upgrade.Release
	Newer      bool
	Homebrew   bool
	Brew       string
	CheckErr   string
	Restarting string
	// Manual says nothing restarts the daemon here: no service runs it.
	Manual bool
}

// available is the release the status line offers, when the daemon runs
// an older one.
func (s *Server) available(running string) *upgrade.Release {
	if s.opts.Updates == nil {
		return nil
	}
	r, ok := s.opts.Updates.Cached()
	if !ok || !upgrade.Newer(running, r.Version) {
		return nil
	}
	return &r
}

// updatePage shows the running and latest versions and, when a newer
// release is out, how to install it. With updates off there is no update
// page.
func (s *Server) updatePage(w http.ResponseWriter, r *http.Request) {
	if s.opts.Updates == nil {
		http.Error(w, "this view has updates off", http.StatusNotFound)
		return
	}
	p := &page{Title: "Update"}
	v := updateView{Brew: upgrade.BrewCommand, Restarting: r.URL.Query().Get("restarting")}
	ctx, cancel := call(r)
	defer cancel()
	if h, err := s.src.Health(ctx); err == nil {
		v.Running = h.Version
	}
	rel, err := s.opts.Updates.Latest(ctx)
	if err != nil {
		v.CheckErr = errText(err)
	}
	v.Latest, v.Homebrew = rel, s.opts.Updates.Homebrew()
	v.Newer = upgrade.Newer(v.Running, rel.Version)
	v.Manual = s.opts.Service == nil || !s.opts.Service.Running()
	if v.Restarting != "" && v.Running == v.Restarting {
		v.Restarting = ""
	}
	s.mu.Lock()
	if s.restartErr != "" {
		p.Err, s.restartErr, v.Restarting = s.restartErr, "", ""
	}
	s.mu.Unlock()
	s.render(w, r, "update", p, v)
}

// update asks the daemon to install the release the page offered, as the
// person signed in, and restarts the background service after answering,
// since the restart may stop this process. The daemon refuses anyone
// without the admin role, and a page that offered a release that is no
// longer the latest.
func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	if s.opts.Updates == nil {
		http.Error(w, "this view has updates off", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), installTimeout)
	defer cancel()
	rel, err := s.src.InstallRelease(ctx, r.PostFormValue("version"), s.opts.NewKey())
	if err != nil {
		flash(w, "Update failed: "+errText(err), true)
		http.Redirect(w, r, "/update", http.StatusSeeOther)
		return
	}
	if s.opts.Service == nil || !s.opts.Service.Running() {
		flash(w, "Installed "+rel.Version+". Restart open-conduit serve to run it.", false)
		http.Redirect(w, r, "/update", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/update?restarting="+rel.Version, http.StatusSeeOther)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	svc := s.opts.Service
	go func() {
		if err := svc.Restart(); err != nil {
			s.restartFailed(err)
		}
	}()
}

// restartFailed is called when a restart couldn't be asked for; the page
// keeps saying it is restarting until the daemon reports the new version,
// so it is noted for the next page instead.
func (s *Server) restartFailed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restartErr = "Restarting the service failed: " + errText(err) + ". Run open-conduit service stop, then open-conduit service start."
}
