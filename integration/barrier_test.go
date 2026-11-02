package integration_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests never wait on real time (CONTRIBUTING.md, "Tests"). They wait on a
// condition with eventually, the one place integration tests poll, or, to
// show that something did not happen, on the daemon's loop barrier with
// drill.settle. TestNoSleep refuses new time.Sleep calls.

// eventually polls ok until it holds or timeout passes and reports whether it
// held. The poll interval starts short and backs off, so a cheap condition
// is seen at once and an expensive one (a docker inspect) is not hammered.
func eventually(timeout time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(timeout)
	interval := 5 * time.Millisecond
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		<-timer.C
		if interval < 100*time.Millisecond {
			interval *= 2
		}
		timer.Reset(interval)
	}
	return true
}

// waitWithin fails the test unless ok holds within timeout.
func waitWithin(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	if !eventually(timeout, ok) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitFor fails the test unless ok holds within 30 seconds.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitWithin(t, what, 30*time.Second, ok)
}

// waitUntil fails the test unless ok holds within 10 seconds.
func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitWithin(t, what, 10*time.Second, ok)
}

// passFile is where a drill's faultinject daemon publishes its completed
// loop passes (OPEN_CONDUIT_PASS_FILE; see internal/bootstrap/fault_inject.go).
func (d *drill) passFile() string { return filepath.Join(d.root, "loop-passes") }

// env is a drill daemon's environment: the armed fault, if any, and the
// loop barrier's file, cleared so the count is the new process's.
func (d *drill) env(fault string) []string {
	_ = os.Remove(d.passFile())
	return append(os.Environ(), "OPEN_CONDUIT_FAULT="+fault, "OPEN_CONDUIT_PASS_FILE="+d.passFile())
}

// passes is the number of host loop passes the running daemon has completed.
func (d *drill) passes() int {
	b, err := os.ReadFile(d.passFile())
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// settlePasses is how many full loop passes settle waits for: the first
// routes what was accepted before the call, the second starts or projects
// what that routing made due, and the third leaves a margin.
const settlePasses = 3

// settle waits until the daemon has completed settlePasses full loop passes
// that began after the call, so everything the host loop does with the
// state at the call (routing, starting ready Work, projecting to trackers
// and Slack, reconciling resources) has been done. A test then checks that
// nothing further happened. It does not cover an executor's process, which
// runs outside the loop: wait on that process's own effect with eventually.
func (d *drill) settle() {
	d.t.Helper()
	// The pass in progress at the call may have begun before it.
	target := d.passes() + 1 + settlePasses
	d.until("the daemon's loop passes (needs the faultinject build)", 30*time.Second, func() bool { return d.passes() >= target })
}
