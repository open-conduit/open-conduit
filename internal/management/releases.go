package management

import (
	"context"
	"time"
)

// Releases checks for and installs releases of the daemon's own binary.
type Releases interface {
	// Latest is the latest release's version, asked for now.
	Latest(ctx context.Context) (string, error)
	// Install puts version in place of the daemon's binaries. It doesn't
	// restart anything: the service manager or the operator does.
	Install(ctx context.Context, version string) error
}

// installTimeout bounds checking for, downloading and installing a release.
const installTimeout = 5 * time.Minute

// installRelease is POST /releases/install: put the latest release in
// place of the daemon's binaries. It needs admin, is refused over the
// network, and installs only the version the caller names when it is
// still the latest, so a stale page installs nothing. Each call is
// recorded in the operator trail, refused or not.
func (s *Server) installRelease(c *call) (int, any, error) {
	var in struct {
		Version string `json:"version"`
	}
	var err error
	defer func() { s.observeOperator(c, "operator.release_installed", in.Version, "", err) }()
	if err = c.decode(&in); err != nil {
		return 0, nil, err
	}
	c.note("version", in.Version)
	if s.o.Releases == nil {
		err = fail(404, "updates_off", "this daemon doesn't install releases: its binary is managed some other way (serve --no-updates)")
		return 0, nil, err
	}
	if in.Version == "" {
		err = fail(400, "malformed", "version is required: the release to install, which must be the latest")
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(c.ctx, installTimeout)
	defer cancel()
	latest, lerr := s.o.Releases.Latest(ctx)
	if lerr != nil {
		err = fail(502, "release_check_failed", "couldn't check for a release: "+lerr.Error())
		return 0, nil, err
	}
	if latest != in.Version {
		err = fail(409, "stale_release", "the latest release is now "+latest+"; check it and try again")
		return 0, nil, err
	}
	if ierr := s.o.Releases.Install(ctx, latest); ierr != nil {
		err = fail(502, "install_failed", ierr.Error())
		return 0, nil, err
	}
	return 200, map[string]any{"version": latest}, nil
}
