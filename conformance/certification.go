package conformance

// Extension certification (07-03): the public contract between an extension
// package and the conformance runner (`open-conduit extension certify DIR`).
// A package ships a fixture (protocol.Fixture) describing how to exercise its declared
// surfaces against fake services; the runner starts the package exactly as
// the daemon does, runs the criteria below against it and reports, for the
// tested protocol, host and package versions, which passed, which failed and
// what was not covered.

// CriteriaVersion versions the certification criteria. A report states it;
// a package certified under one version is not implied certified under a
// later one.
const CriteriaVersion = 3

// Criterion is one certification check.
type Criterion struct {
	ID string
	// Surface is the manifest surface the criterion certifies: lifecycle,
	// storage, ingress, routes, operations, executors or resources.
	Surface string
	// Applies says when the criterion is required ("always" or the manifest
	// declaration that makes it apply).
	Applies     string
	Requirement string
}

// Criteria are the version 1 certification criteria. A package is certified
// only when every criterion that applies to its manifest passes; a skipped
// criterion is one that does not apply.
var Criteria = []Criterion{
	{"manifest.valid", "lifecycle", "always", "The package layout and manifest are valid offline: strict manifest rules, the directory named after the manifest, a relative executable command inside the package."},
	{"handshake.manifest", "lifecycle", "always", "initialize negotiates a version both sides list and returns the manifest file's content; configure with the fixture's entries succeeds and registers only the extension's own sources, families and webhook paths."},
	{"handshake.version_refusal", "lifecycle", "always", "initialize offering only versions the package does not speak is refused with unsupported_protocol_version."},
	{"lifecycle.before_initialize", "lifecycle", "always", "A request before initialize is refused, never served."},
	{"lifecycle.unknown_method", "lifecycle", "always", "An unknown method is answered method_not_found and the connection stays usable."},
	{"lifecycle.shutdown", "lifecycle", "always", "After shutdown, new requests are answered shutting_down."},
	{"lifecycle.stdin_eof", "lifecycle", "always", "The process exits within 5 seconds of its stdin closing (a host crash never orphans it)."},
	{"lifecycle.restart", "lifecycle", "always", "Killed abruptly, the package is restarted by the host and completes initialize and configure again from its state directory."},
	{"transport.clean_stdout", "lifecycle", "always", "Nothing but protocol messages is written to stdout during the whole run."},
	{"scope.host_surface", "lifecycle", "always", "No extension-to-host call is refused as unauthorized during the run: the package stays within its namespaces, configured scopes and registered sources."},
	{"storage.kv_migration", "storage", "kv_version > 0", "configure from stored KV layout 0 migrates to the manifest's kv_version, and repeating the migration from the same stored version (a crash mid-migration) succeeds again."},
	{"ingress.conformance", "ingress", "an ingress extension type", "The shared ingress suite (conformance.CheckIngress) passes on the fixture's source: stable push normalization, push and reconcile agree on identity, complete paging and declared loss for one-shot families (a source whose families are all re-derivable needs no one-shot case)."},
	{"routes.decisions", "routes", "route handlers and route fixture cases", "Each route fixture case is decided deterministically (twice, identically) as the fixture expects, creating only declared Work kinds, with any Work display within its limits and linking only to http or https URLs (extension.ValidateDisplay)."},
	{"operations.cases", "operations", "operations and operation fixture cases", "Each operation fixture case returns the expected output or refusal code, and an undeclared operation is refused."},
	{"executor.conformance", "executors", "an executor", "The shared executor suite (conformance.CheckExecutor) passes: recovery metadata before acknowledgement, adoption without replacement, an unknown state not adoptable and persistence failures propagated."},
	{"executor.inject", "executors", "an executor with inject", "The shared inject suite (conformance.CheckInjectExecutor) passes on the fixture's inject case."},
	{"executor.controls", "executors", "an executor with stop or forget", "Declared stop and forget controls are served for an execution the package does not hold."},
	{"executor.resource_mode", "executors", "an executor with resource_mode", "Each executor mode fixture case answers validate executor_mode with the mode the case expects, twice identically, and every executor declaring resource_mode has a case."},
	{"resources.workspace", "resources", "a resource type with mode_default or sandbox", "Each resource fixture case is answered as it expects, twice identically: validate resource_mode names the mode the config writes (mode_default), and validate resource_sandbox refuses exactly the configs a sandboxed binding can't use, with a reason (sandbox). Every such type has a case."},
	{"surfaces.served", "lifecycle", "checks or tasks", "Every declared check answers check.run and every declared task is served by task.run."},
}
