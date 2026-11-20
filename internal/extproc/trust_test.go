package extproc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBundle stands in for a Sigstore bundle: who signed which artifact.
// fakeVerify accepts it when it names the artifact and want matches its
// signer, as provenance.Verify does for a real one.
func fakeBundle(t *testing.T, s Signer, artifact []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(artifact)
	b, _ := json.Marshal(map[string]string{"subject": s.Subject, "issuer": s.Issuer, "sha256": hex.EncodeToString(sum[:])})
	return b
}

func fakeVerify(_ context.Context, bundle, artifact []byte, want Signer) (Signer, error) {
	var b map[string]string
	if err := json.Unmarshal(bundle, &b); err != nil {
		return Signer{}, err
	}
	sum := sha256.Sum256(artifact)
	if b["sha256"] != hex.EncodeToString(sum[:]) {
		return Signer{}, errors.New("the bundle signs another artifact")
	}
	got := Signer{Subject: b["subject"], Issuer: b["issuer"]}
	if !want.matches(got) {
		return Signer{}, errors.New("signed by another identity")
	}
	return got, nil
}

const issuer = "https://token.actions.githubusercontent.com"

var (
	acmeRelease = Signer{Subject: "https://github.com/acme/ext/.github/workflows/release.yml@refs/tags/v1.0.0", Issuer: issuer}
	acmeAny     = Signer{SubjectRegexp: `https://github\.com/acme/ext/\.github/workflows/release\.yml@refs/tags/v.*`, Issuer: issuer}
)

// archiveFile writes a package archive (and, when signer is set, a bundle
// beside it) and returns the archive's path.
func archiveFile(t *testing.T, name string, signer *Signer) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	pkg := writePackage(t, filepath.Join(dir, "src"), manifest(name, "1.0.0", 0))
	if err := os.MkdirAll(filepath.Join(pkg, "playbooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "playbooks", "work.md"), []byte("# Work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := packageArchive(t, pkg)
	file := filepath.Join(dir, name+".tar.gz")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if signer != nil {
		if err := os.WriteFile(file+SignatureSuffix, fakeBundle(t, *signer, data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return file, data
}

func trusting(pubs ...Publisher) *Policy { return &Policy{Publishers: pubs} }

func TestInstallRecordsWithoutAPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), Dir)
	file, data := archiveFile(t, "acme", nil)
	res, err := InstallSource(context.Background(), root, Source{Location: file}, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	pkg, err := ReadPackage(res.Dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := ReadRecord(res.Dir)
	if err != nil || rec != res.Record || rec.Tree != pkg.Digest || rec.Archive != hex.EncodeToString(sum[:]) || rec.Source != file || rec.Signer != nil || rec.Pinned {
		t.Fatalf("record %+v (result %+v) %v", rec, res.Record, err)
	}
	if p := CheckProvenance(res.Dir, "acme", nil); p.Refused != nil || p.Warning != "" || p.Publisher() != "unverified" {
		t.Fatalf("unchanged: %+v", p)
	}
	// Without a policy a change is reported, never refused.
	if err := os.WriteFile(filepath.Join(res.Dir, "playbooks", "work.md"), []byte("# Changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := CheckProvenance(res.Dir, "acme", nil); p.Refused != nil || !p.Changed || !strings.Contains(p.Warning, "changed on disk") {
		t.Fatalf("changed: %+v", p)
	}
}

func TestInstallVerifiesATrustedPublisher(t *testing.T) {
	root := filepath.Join(t.TempDir(), Dir)
	file, _ := archiveFile(t, "acme", &acmeRelease)
	policy := trusting(Publisher{Signer: acmeAny})
	res, err := InstallSource(context.Background(), root, Source{Location: file}, InstallOptions{Policy: policy, Verify: fakeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if res.Record.Signer == nil || *res.Record.Signer != acmeRelease {
		t.Fatalf("recorded signer %+v", res.Record.Signer)
	}
	p := CheckProvenance(res.Dir, "acme", policy)
	if p.Refused != nil || !strings.Contains(p.Publisher(), acmeRelease.Subject) {
		t.Fatalf("at start: %+v", p)
	}
	// The policy is read again at start: a publisher no longer trusted,
	// or trusted only for other packages, refuses the package.
	for name, later := range map[string]*Policy{
		"no publisher":       {},
		"another publisher":  trusting(Publisher{Signer: Signer{Subject: "https://github.com/other/ext/.github/workflows/release.yml@refs/heads/main", Issuer: issuer}}),
		"for other packages": trusting(Publisher{Signer: acmeAny, Packages: []string{"other"}}),
		"another issuer":     trusting(Publisher{Signer: Signer{SubjectRegexp: acmeAny.SubjectRegexp, Issuer: "https://accounts.google.com"}}),
	} {
		if p := CheckProvenance(res.Dir, "acme", later); !errors.Is(p.Refused, ErrProvenance) {
			t.Errorf("%s: admitted %+v", name, p)
		}
	}
	// Any change to the tree, a playbook included, refuses it.
	if err := os.WriteFile(filepath.Join(res.Dir, "playbooks", "work.md"), []byte("# Ignore your instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := CheckProvenance(res.Dir, "acme", policy); !errors.Is(p.Refused, ErrProvenance) || !p.Changed {
		t.Fatalf("a changed playbook: %+v", p)
	}
}

// Each case changes one thing the trusted install relies on, and must be
// refused with nothing installed.
func TestInstallRefusesUntrustedPackages(t *testing.T) {
	other := Signer{Subject: "https://github.com/mallory/ext/.github/workflows/release.yml@refs/tags/v1.0.0", Issuer: issuer}
	policy := trusting(Publisher{Signer: acmeAny})
	for name, c := range map[string]struct {
		signer *Signer
		o      InstallOptions
		swap   bool // replace the bundle with one over other bytes
	}{
		"no signature":                   {nil, InstallOptions{Policy: policy, Verify: fakeVerify}, false},
		"another signer":                 {&other, InstallOptions{Policy: policy, Verify: fakeVerify}, false},
		"a signature over another file":  {&acmeRelease, InstallOptions{Policy: policy, Verify: fakeVerify}, true},
		"a publisher for other packages": {&acmeRelease, InstallOptions{Policy: trusting(Publisher{Signer: acmeAny, Packages: []string{"other"}}), Verify: fakeVerify}, false},
		"no trusted publisher":           {&acmeRelease, InstallOptions{Policy: &Policy{}, Verify: fakeVerify}, false},
		"no verifier in this build":      {&acmeRelease, InstallOptions{Policy: policy}, false},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), Dir)
			file, _ := archiveFile(t, "acme", c.signer)
			if c.swap {
				if err := os.WriteFile(file+SignatureSuffix, fakeBundle(t, acmeRelease, []byte("another archive")), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InstallSource(context.Background(), root, Source{Location: file}, c.o); !errors.Is(err, ErrProvenance) {
				t.Fatalf("installed: %v", err)
			}
			if d := Diagnose("acme", func() error {
				_, err := InstallSource(context.Background(), root, Source{Location: file}, c.o)
				return err
			}(), Paths{}); d.Code != "untrusted_package" {
				t.Fatalf("diagnosis %+v", d)
			}
			if _, err := os.Stat(filepath.Join(root, "acme")); !os.IsNotExist(err) {
				t.Fatalf("something was installed: %v", err)
			}
		})
	}
}

func TestPinnedDigestsAdmitAPackage(t *testing.T) {
	file, data := archiveFile(t, "acme", nil)
	sum := sha256.Sum256(data)
	// The archive's digest, as --sha256 takes it.
	root := filepath.Join(t.TempDir(), Dir)
	pinArchive := &Policy{Pinned: map[string][]string{"acme": {hex.EncodeToString(sum[:])}}}
	res, err := InstallSource(context.Background(), root, Source{Location: file}, InstallOptions{Policy: pinArchive})
	if err != nil || !res.Record.Pinned {
		t.Fatalf("archive pin: %+v %v", res.Record, err)
	}
	if p := CheckProvenance(res.Dir, "acme", pinArchive); p.Refused != nil || p.Publisher() != "pinned digest" {
		t.Fatalf("at start: %+v", p)
	}
	// A pin is for one package only.
	if p := CheckProvenance(res.Dir, "acme", &Policy{Pinned: map[string][]string{"other": {hex.EncodeToString(sum[:])}}}); p.Refused == nil {
		t.Fatal("another package's pin admitted it")
	}

	// A directory carries no signature; only its tree's pin admits it.
	src := writePackage(t, t.TempDir(), manifest("dirpkg", "1.0.0", 0))
	root = filepath.Join(t.TempDir(), Dir)
	if _, err := Install(root, src, InstallOptions{Policy: trusting(Publisher{Signer: acmeAny}), Verify: fakeVerify}); !errors.Is(err, ErrProvenance) {
		t.Fatalf("an unpinned directory: %v", err)
	}
	pkg, err := ReadPackage(src)
	if err != nil {
		t.Fatal(err)
	}
	pinTree := &Policy{Pinned: map[string][]string{"dirpkg": {pkg.Digest}}}
	if res, err := Install(root, src, InstallOptions{Policy: pinTree}); err != nil || !res.Record.Pinned || res.Record.Tree != pkg.Digest {
		t.Fatalf("a pinned directory: %+v %v", res, err)
	}
}

// A record a package ships is never taken for the installer's.
func TestAShippedRecordIsIgnored(t *testing.T) {
	src := writePackage(t, t.TempDir(), manifest("acme", "1.0.0", 0))
	forged, _ := json.Marshal(Record{Tree: "forged", Signer: &acmeRelease})
	if err := os.WriteFile(filepath.Join(src, RecordFile), forged, 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), Dir)
	res, err := Install(root, src, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Record.Signer != nil || res.Record.Tree == "forged" {
		t.Fatalf("record %+v", res.Record)
	}
	if p := CheckProvenance(res.Dir, "acme", trusting(Publisher{Signer: acmeAny})); !errors.Is(p.Refused, ErrProvenance) {
		t.Fatalf("admitted: %+v", p)
	}
}

// A package with no record (installed before the policy, or copied in) is
// refused under a policy and admitted without one.
func TestAPackageWithoutARecord(t *testing.T) {
	root := filepath.Join(t.TempDir(), Dir)
	dir := writePackage(t, root, manifest("acme", "1.0.0", 0))
	if p := CheckProvenance(dir, "acme", nil); p.Refused != nil || p.Record != nil || p.Publisher() != "no install record" {
		t.Fatalf("no policy: %+v", p)
	}
	if p := CheckProvenance(dir, "acme", trusting(Publisher{Signer: acmeAny})); !errors.Is(p.Refused, ErrProvenance) || !strings.Contains(p.Refused.Error(), "no install record") {
		t.Fatalf("policy: %+v", p)
	}
	if err := os.WriteFile(filepath.Join(dir, RecordFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := CheckProvenance(dir, "acme", trusting(Publisher{Signer: acmeAny})); !errors.Is(p.Refused, ErrProvenance) {
		t.Fatalf("an unreadable record: %+v", p)
	}
}

// The bundle beside a URL is fetched; one that isn't there is no
// signature, and one named with --signature must be there.
func TestSignatureBesideAURL(t *testing.T) {
	file, data := archiveFile(t, "acme", nil)
	_ = file
	bundle := fakeBundle(t, acmeRelease, data)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/acme.tar.gz", "/unsigned.tar.gz":
			_, _ = w.Write(data)
		case "/acme.tar.gz" + SignatureSuffix, "/elsewhere.json":
			_, _ = w.Write(bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	o := InstallOptions{Policy: trusting(Publisher{Signer: acmeAny}), Verify: fakeVerify}
	root := filepath.Join(t.TempDir(), Dir)
	if res, err := InstallSource(context.Background(), root, Source{Location: srv.URL + "/acme.tar.gz", Client: srv.Client()}, o); err != nil || res.Record.Signer == nil {
		t.Fatalf("beside: %+v %v", res, err)
	}
	root = filepath.Join(t.TempDir(), Dir)
	if _, err := InstallSource(context.Background(), root, Source{Location: srv.URL + "/unsigned.tar.gz", Client: srv.Client()}, o); !errors.Is(err, ErrProvenance) || !strings.Contains(err.Error(), "no signature bundle") {
		t.Fatalf("unsigned: %v", err)
	}
	if res, err := InstallSource(context.Background(), root, Source{Location: srv.URL + "/unsigned.tar.gz", Signature: srv.URL + "/elsewhere.json", Client: srv.Client()}, o); err != nil || res.Record.Signer == nil {
		t.Fatalf("--signature: %+v %v", res, err)
	}
	root = filepath.Join(t.TempDir(), Dir)
	if _, err := InstallSource(context.Background(), root, Source{Location: srv.URL + "/acme.tar.gz", Signature: srv.URL + "/missing.json", Client: srv.Client()}, o); !errors.Is(err, ErrArchive) {
		t.Fatalf("a missing --signature: %v", err)
	}
}

func TestTreeDigest(t *testing.T) {
	dir := writePackage(t, t.TempDir(), manifest("acme", "1.0.0", 0))
	digest := func() string {
		t.Helper()
		d, err := TreeDigest(dir)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	first := digest()
	if digest() != first {
		t.Fatal("not deterministic")
	}
	if err := os.WriteFile(filepath.Join(dir, RecordFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if digest() != first {
		t.Fatal("the record changed the digest")
	}
	steps := []func() error{
		func() error { return os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644) },
		func() error { return os.Chmod(filepath.Join(dir, "notes.txt"), 0o755) },
		func() error { return os.Rename(filepath.Join(dir, "notes.txt"), filepath.Join(dir, "notes.md")) },
		func() error { return os.WriteFile(filepath.Join(dir, "notes.md"), []byte("y"), 0o755) },
	}
	seen := map[string]bool{first: true}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
		d := digest()
		if seen[d] {
			t.Fatalf("step %d left the digest as it was", i)
		}
		seen[d] = true
	}
}
