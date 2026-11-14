package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// leftoverDocker is a docker CLI stand-in whose daemon holds three of
// Open Conduit's networks (two this host's, one another's) among five,
// and refuses to create another for want of address space. Each network
// rm is recorded in rms.
func leftoverDocker(t *testing.T) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()
	script := `d=` + dir + `
case "$3 $4" in
"network create") echo "Error response from daemon: all predefined address pools have been fully subnetted" >&2; exit 1;;
"network ls") case "$*" in
  *label=dev.open-conduit.network*) printf 'oc-a\noc-b\noc-c\n';;
  *name=oc-c*) echo oc-c;;
  *label=dev.open-conduit.host=host-1*) printf 'oc-a\noc-b\n';;
  *) printf 'bridge\nhost\nnone\noc-a\noc-b\noc-c\n';;
  esac;;
"network inspect") shift 6; for n; do case "$n" in
  oc-new) echo "Error response from daemon: network oc-new not found" >&2; exit 1;;
  oc-c) echo '"oc-c" {"dev.open-conduit.host":"host-2","dev.open-conduit.execution":"x9","dev.open-conduit.network":"execution"}';;
  *) echo '"'$n'" {"dev.open-conduit.host":"host-1","dev.open-conduit.execution":"x1","dev.open-conduit.network":"execution"}';;
  esac; done;;
"network rm") echo "$5" >> "$d/rms";;
esac`
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return New(Options{Docker: bin, Host: "host-1", Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH")} }}), dir
}

// TestNetworkAddressSpaceExhausted: a network create that fails for want
// of address space says how many of the context's networks are Open
// Conduit's and that leftovers of ended runs are the likely cause (#637).
func TestNetworkAddressSpaceExhausted(t *testing.T) {
	p, _ := leftoverDocker(t)
	err := p.network(context.Background(), "default", "oc-new", map[string]string{extension.ContainerLabelHost: "host-1"}, true)
	if err == nil {
		t.Fatal("created a network with no address space")
	}
	for _, want := range []string{"fully subnetted", "3 (2 of them this daemon's) of its 6 networks are Open Conduit's", "leftovers of ended runs", "network ls --filter label=dev.open-conduit.network"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if !addressSpaceExhausted(err) || addressSpaceExhausted(os.ErrNotExist) {
		t.Error("addressSpaceExhausted misreads")
	}
}

// TestRemoveObjectIsTheHostsOwn: Remove takes a listed network only once
// its labels show it is this host's and an execution's; another host's is
// refused and left (#637).
func TestRemoveObjectIsTheHostsOwn(t *testing.T) {
	p, dir := leftoverDocker(t)
	ctx := context.Background()
	if err := p.Remove(ctx, extension.ContainerRef{ID: "default/network:oc-c"}); err == nil {
		t.Error("removed another host's network")
	}
	if _, err := os.Stat(filepath.Join(dir, "rms")); err == nil {
		t.Fatal("ran network rm on another host's network")
	}
	made, err := p.made(ctx, "default", "daemon-1")
	if err != nil || len(made) != 2 {
		t.Fatalf("listed %+v %v, want this host's two networks", made, err)
	}
	for _, st := range made {
		if st.Running || st.Ref.Labels[extension.ContainerLabelExecution] != "x1" || st.Ref.Labels[labelRole] != objectNetwork {
			t.Errorf("listed %+v, want a stopped network entry of x1", st)
		}
	}
}
