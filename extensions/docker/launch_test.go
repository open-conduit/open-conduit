package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// recordingDocker is a docker CLI stand-in that records each call's
// arguments, one line each, and fails any call whose arguments contain
// fail, with Docker's words.
func recordingDocker(t *testing.T, fail string) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `echo "$*" >> ` + log + `
case "$*" in *` + fail + `*) echo "Error response from daemon: all predefined address pools have been fully subnetted" >&2; exit 1;; esac`
	if fail == "" {
		script = `echo "$*" >> ` + log
	}
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return New(Options{Docker: bin, Host: "host-1", Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH")} }}), log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// TestRemoveTakesTheLaunchsOwnNetworks (#669): removing a segment's
// container removes its gateway, volume and the two networks named after
// the launch, never the execution's other segments' or the network
// releases before 6.7 shared between them, so reconcile can release a
// parked execution's while its wake's launch makes new ones.
func TestRemoveTakesTheLaunchsOwnNetworks(t *testing.T) {
	p, log := recordingDocker(t, "")
	labels := map[string]string{extension.ContainerLabelHost: "host-1", extension.ContainerLabelExecution: "x1", extension.ContainerLabelSegment: "2"}
	if err := p.Remove(context.Background(), extension.ContainerRef{ID: "default/abc", Labels: labels}); err != nil {
		t.Fatal(err)
	}
	launch := LaunchNetwork("host-1", "x1", "2")
	var removed []string
	for _, c := range calls(t, log) {
		if rest, ok := strings.CutPrefix(c, "--context default network rm "); ok {
			removed = append(removed, rest)
		}
	}
	if len(removed) != 2 || removed[0] != launch || removed[1] != launch+"-egress" {
		t.Errorf("removed networks %v, want %s and its egress network", removed, launch)
	}
	for _, c := range calls(t, log) {
		if strings.Contains(c, LaunchNetwork("host-1", "x1", "3")) || strings.HasSuffix(c, " "+ExecutionNetwork("host-1", "x1")) {
			t.Errorf("%q touches another launch's", c)
		}
	}
	if args := networkArgs(launch, labels, true); !strings.Contains(strings.Join(args, " "), extension.ContainerLabelSegment+"=2") {
		t.Errorf("a launch's network isn't labelled with its segment: %v", args)
	}
}

// TestCreateFailureSaysWhy (#669): a create that Docker refuses is a
// sandbox refusal carrying Docker's own words, so the cause reaches the
// host across the protocol rather than an internal error with none.
func TestCreateFailureSaysWhy(t *testing.T) {
	p, _ := recordingDocker(t, "info")
	data, _ := json.Marshal(Plan{Context: "default", Argv: []string{"true"}, EnvFile: filepath.Join(t.TempDir(), "env")})
	_, err := p.Create(context.Background(), extension.ContainerPlan{Image: "img", Data: domain.Payload{Schema: PlanSchema, Data: data}},
		map[string]string{extension.ContainerLabelHost: "host-1", extension.ContainerLabelExecution: "x1", extension.ContainerLabelSegment: "1"})
	if !errors.Is(err, extension.ErrSandboxUnavailable) || !strings.Contains(err.Error(), "fully subnetted") {
		t.Errorf("create failed with %v, want sandbox_unavailable with Docker's words", err)
	}
}
