// Package runtimetest drives the production runtime (router, scheduler,
// waits, timers and causal derivation) over SQLite in a temp dir with the
// conformance clock, as conformance.RuntimeDriver fixtures. It is for tests
// only: the runtime's own in-package tests run the shared runtime suites
// through it, and the integration tests build on the same fixtures.
package runtimetest
