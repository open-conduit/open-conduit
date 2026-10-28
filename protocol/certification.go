package protocol

// Package certification fixture (07-03): the contract between an extension
// package and the conformance runner (`open-conduit extension certify DIR`,
// criteria in package conformance). It lives here, beside the manifest,
// because a package ships it as certification.json.

// CertificationFile is the package file that holds the fixture or names the
// command that prints it.
type CertificationFile struct {
	Fixture *Fixture `json:"fixture,omitempty"`
	// Command, relative to the package, prints the fixture as one JSON
	// line on stdout and keeps serving until its stdin closes.
	Command []string `json:"command,omitempty"`
}

// Fixture is a package's certification fixture: the configuration to apply
// and one case per surface. It is either inline in certification.json or
// printed (one JSON line on stdout) by the file's fixture command, which the
// runner starts in a fresh temporary directory and keeps running (serving
// fake services) until the run ends. The string "{tmp}" anywhere in the
// fixture is replaced by that directory and "{state}" by the extension's
// state directory.
type Fixture struct {
	// Entries are sent in configure. Their scopes are created in the
	// runner's store and configured for the extension.
	Entries []ConfigEntry `json:"entries,omitempty"`
	// Secrets are the values the runner's resolver returns for secret names
	// bound to the entries (selected names and their connections' names).
	Secrets map[string]string `json:"secrets,omitempty"`
	// Scopes are extra scopes configured for the extension.
	Scopes     []string        `json:"scopes,omitempty"`
	Ingress    *IngressCase    `json:"ingress,omitempty"`
	Routes     []RouteCase     `json:"routes,omitempty"`
	Operations []OperationCase `json:"operations,omitempty"`
	Executor   *ExecutorCase   `json:"executor,omitempty"`
	Inject     *InjectCase     `json:"inject,omitempty"`
}

// IngressCase drives CheckIngress: Source names a source the fixture's
// configure registers.
type IngressCase struct {
	Source    string      `json:"source"`
	Delivery  Delivery    `json:"delivery"`
	Canonical Observation `json:"canonical"`
	// OneShotType is the declared non-re-derivable family push yields. It
	// is empty only when every declared family is re-derivable.
	OneShotType string    `json:"one_shot_type"`
	OneShot     *Delivery `json:"one_shot,omitempty"`
	// ReceiveTimeVaries declares that the package stamps reconcile
	// observations with its own clock's receive time (Slack does), which a
	// separate process cannot share with the fixture: the runner then
	// compares observations with received_at cleared, and says so.
	ReceiveTimeVaries bool `json:"receive_time_varies,omitempty"`
}

// RouteCase is one routing decision the package must make.
type RouteCase struct {
	Handler string        `json:"handler"`
	Input   RouteInput    `json:"input"`
	Want    RouteDecision `json:"want"`
}

// OperationCase is one toolbelt operation call: Output is the expected
// output, or Error the expected refusal code. The runner calls with an
// authority for Work "certify-work" in the first configured scope.
type OperationCase struct {
	Operation string   `json:"operation"`
	Input     Payload  `json:"input"`
	Output    *Payload `json:"output,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// ExecutorCase is the binding config CheckExecutor runs an attempt with.
// The attempt must finish on its own.
type ExecutorCase struct {
	Executor string  `json:"executor"`
	Config   Payload `json:"config"`
}

// InjectCase drives CheckInjectExecutor: the attempt runs until FinishFile
// is created by the runner, and is ready for input once ReadyFile exists
// (when set).
type InjectCase struct {
	Executor   string  `json:"executor"`
	Config     Payload `json:"config"`
	ReadyFile  string  `json:"ready_file,omitempty"`
	FinishFile string  `json:"finish_file"`
}
