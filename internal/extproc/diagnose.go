package extproc

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/open-conduit/open-conduit/protocol"
)

// Diagnostic is an operator-facing explanation of an extension that could
// not be admitted or started: a stable code, what happened and what to do.
// It never carries config values or secrets; Detail is the wrapped error
// text, which names files, versions and protocol codes only.
type Diagnostic struct {
	Extension string `json:"extension,omitempty"`
	Code      string `json:"code"`
	Summary   string `json:"summary"`
	Hint      string `json:"hint"`
	Detail    string `json:"detail,omitempty"`
	// Docs links the troubleshooting section that explains the code.
	Docs string `json:"docs,omitempty"`
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("%s: %s (%s). %s. See %s", d.Extension, d.Summary, d.Code, d.Hint, d.Docs)
}

// Diagnose classifies a discovery, conflict, install or start failure.
// paths, when set, names the extension's log for handshake failures.
func Diagnose(name string, err error, paths Paths) Diagnostic {
	d := Diagnostic{Extension: name, Detail: err.Error(), Docs: Troubleshooting}
	log := "the extension's log"
	if paths.Log != "" {
		log = paths.Log
	}
	switch {
	case errors.Is(err, protocol.ErrIncompatible), errors.Is(err, ErrIncompatiblePackage) && !errors.Is(err, protocol.ErrKVDowngrade):
		d.Code, d.Summary = "incompatible_protocol", "the package and this host share no protocol version or layout"
		d.Hint = fmt.Sprintf("this host speaks protocol %v; install a release of the package built for it, or upgrade Open Conduit", protocol.Versions)
	case errors.Is(err, ErrPlatform):
		d.Code, d.Summary = "wrong_platform", "the package is built for another OS or architecture"
		d.Hint = fmt.Sprintf("install the release archive built for %s/%s", runtime.GOOS, runtime.GOARCH)
	case errors.Is(err, ErrArchive):
		d.Code, d.Summary = "invalid_archive", "the release archive was refused"
		d.Hint = "check the URL or path and its --sha256; an archive is a .tar.gz holding one package directory of regular files"
	case errors.Is(err, ErrManifestMismatch):
		d.Code, d.Summary = "manifest_mismatch", "the process announced a manifest that differs from its manifest file"
		d.Hint = "the binary and open-conduit-extension.json come from different releases; reinstall the package"
	case errors.Is(err, protocol.ErrKVDowngrade):
		d.Code, d.Summary = "kv_downgrade", "the package declares an older KV layout than a newer release already migrated to"
		d.Hint = "reinstall the newer release (open-conduit extension rollback), or restore a backup taken before the upgrade"
	case errors.Is(err, ErrConflict):
		d.Code, d.Summary = "registration_conflict", "the package claims a name another registration owns"
		d.Hint = "remove the conflicting package; first-party names (slack, shell, …) are registered in-process and cannot be installed alongside"
	case errors.Is(err, protocol.ErrNewerManifest):
		d.Code, d.Summary = "needs_newer_host", "the package needs a newer Open Conduit than this one"
		d.Hint = "upgrade Open Conduit to the release the package was built against or later, or install a release of the package built for this one"
	case errors.Is(err, protocol.ErrManifest), errors.Is(err, ErrPackage):
		d.Code, d.Summary = "invalid_package", "the package layout or manifest is invalid"
		d.Hint = "run open-conduit extension check DIR: it lists every rule the package breaks and what to change"
	case errors.Is(err, ErrStart):
		d.Code, d.Summary = "start_failed", "the manifest's command could not be started"
		d.Hint = "check that the command exists in the package and is executable for this platform"
	case errors.Is(err, ErrConfigure):
		d.Code, d.Summary = "configure_refused", "the extension refused the configuration or registered names outside its namespace"
		d.Hint = "check its config entries (open-conduit config check) and " + log
	case errors.Is(err, ErrHandshake), errors.Is(err, protocol.ErrTransport), errors.Is(err, context.DeadlineExceeded):
		d.Code, d.Summary = "handshake_failed", "the process exited or did not answer initialize/configure in time"
		d.Hint = "see " + log + "; the extension must speak the protocol on stdin/stdout and log only to stderr"
	case errors.Is(err, ErrInstalled), errors.Is(err, ErrNotInstalled):
		d.Code, d.Summary = "install_state", err.Error()
		d.Hint = "open-conduit extension list shows what is installed"
	default:
		d.Code, d.Summary, d.Hint = "extension_failed", "the extension failed", "see "+log
	}
	return d
}
