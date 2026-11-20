package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/internal/management"
)

// startNetwork serves the management API on the opt-in management listener
// (Options.Management), with the same router as the owner-only socket. It
// returns a stop that is safe to call more than once; with no listener
// configured it starts nothing.
func (d *daemon) startNetwork(api http.Handler) (func(), error) {
	cfg := d.o.Management
	if !cfg.Enabled() {
		return func() {}, nil
	}
	ln, err := management.ListenNetwork(cfg)
	if err != nil {
		return nil, err
	}
	network := management.NewNetwork(cfg, d.now)
	network.OnThrottle = func(client string) {
		d.log.Warn("management listener refusing a client for too many failed authentications", "client", client, "limit", management.AuthFailureLimit, "window", management.AuthFailureWindow)
	}
	srv := management.NetworkServer(network.Handler(api))
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("management listener stopped", "error", err)
		}
	}()
	mode := "tls"
	switch {
	case cfg.TrustedProxy:
		mode = "plaintext to a trusted local proxy"
	case cfg.ClientCAFile != "":
		mode = "mutual tls"
	}
	d.log.Info("management listener serving", "address", ln.Addr().String(), "mode", mode)
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
	}, nil
}
