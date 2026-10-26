// Package protocol defines the third-party extension subprocess protocol
// extracted in 07-01 from the shipped first-party workloads: the protocol
// version, the manifest and its validation, the method catalogue mapping
// every wire method to the public in-process contract it carries and the
// first-party use that needed it, and the stable error codes.
//
// The transport is JSON-RPC 2.0 over the extension process's stdin and
// stdout, one JSON object per line, symmetric (both sides send requests).
// The decision, its prototype evidence and the lifecycle, cancellation,
// flow-control, crash and error semantics are recorded in
// docs/architecture/extension-protocol.md. The decision is provisional for
// user review; 07-02 implements loading, handshake and supervision against
// this package.
//
// The protocol adds no core primitive: every method is an adapter over an
// existing public contract (package extension, storage KV and reads,
// observability) or a host composition hook the daemon already drives for
// in-process extensions.
package protocol

import "errors"

// Version is the protocol version this module speaks. Versions are integers
// compared for equality; a handshake picks the highest version both sides
// list, and no common version is a deterministic refusal.
const Version = 1

// Versions lists every protocol version this module speaks.
var Versions = []int{Version}

// ErrIncompatible means the two sides share no protocol version.
var ErrIncompatible = errors.New("protocol: no common protocol version")

// Negotiate returns the highest version present in both lists.
func Negotiate(offered, supported []int) (int, error) {
	best := 0
	for _, o := range offered {
		for _, s := range supported {
			if o == s && o > best {
				best = o
			}
		}
	}
	if best == 0 {
		return 0, ErrIncompatible
	}
	return best, nil
}

// Stable application error codes carried in a JSON-RPC error's data.code.
// Codes are [a-z][a-z0-9_]* and secret-free. Transport-level failures use the
// JSON-RPC reserved numeric codes; everything an extension or the host
// authored uses -32000 with one of these (or an extension's own code, for
// operation refusals, verbatim like extension.OperationError).
const (
	CodeUnsupportedVersion = "unsupported_protocol_version" // handshake refusal
	CodeMethodNotFound     = "method_not_found"             // undeclared or unknown method
	CodeInvalid            = "invalid"                      // malformed params or result (extension.ErrInvalid)
	CodeUnsupported        = "unsupported"                  // capability not offered (extension.ErrUnsupported)
	CodeUnauthorized       = "unauthorized"                 // authority, namespace or scope refused (extension.ErrUnauthorized)
	CodeUndeclared         = "undeclared"                   // family, operation or kind not in the manifest (extension.ErrUndeclared)
	CodeNotFound           = "not_found"                    // no such item (storage.ErrNotFound)
	CodeConflict           = "conflict"                     // revision, KV precondition or lease fence failed (storage.ErrConflict)
	CodeCausalLimit        = "causal_limit"                 // chain depth or rate bound (runtime causal limits)
	CodeNotAdoptable       = "not_adoptable"                // executor.recover found nothing to adopt (extension.ErrNotAdoptable)
	CodeNotAccepting       = "not_accepting"                // executor.inject definitely not delivered (extension.ErrNotAccepting)
	CodeNothingToAttach    = "nothing_to_attach"            // executor.attach has nothing yet (extension.ErrNothingToAttach)
	CodeResourceSetup      = "resource_setup_failed"        // resource exists, setup failed (extension.ErrResourceSetup)
	CodeResourceNotFound   = "resource_not_found"           // no owned lease (extension.ErrResourceNotFound)
	CodeRetryAfter         = "retry_after"                  // retryable executor limit; data.reset_at optional (extension.RetryHint)
	CodeReentrant          = "reentrant_call"               // a callback was made while serving an in-transaction method
	CodeOverloaded         = "overloaded"                   // too many requests in flight; retry later
	CodeCancelled          = "cancelled"                    // the caller cancelled the request
	CodeShuttingDown       = "shutting_down"                // the peer is draining after shutdown
	CodeInternal           = "internal"                     // unauthored failure; details stay in the peer's log
	CodeSchema             = "unreadable_schema"            // a payload schema version the running manifest cannot read or write (07-02)
	CodeKVDowngrade        = "kv_downgrade"                 // stored extension KV is newer than the manifest's kv_version (07-02)
)

// ErrTransport means the connection to the peer failed: the process exited,
// a pipe broke, or a malformed or truncated message closed the stream. It is
// never an authored refusal; every request in flight fails with it.
var ErrTransport = errors.New("protocol: transport closed")

// Limits are the provisional framing and flow-control bounds of version 1.
const (
	// MaxMessageBytes bounds one framed JSON message. A longer line is a
	// protocol error that closes the connection. It exceeds the runtime's
	// default reconcile page bound (8 MiB of payload, which is raw JSON in
	// domain.Payload) with room for the envelope; delivery bodies, which are
	// bytes, are base64 and grow by a third.
	MaxMessageBytes = 16 << 20
	// MaxChunkBytes bounds the raw bytes of one artifact.write chunk.
	MaxChunkBytes = 256 << 10
	// MaxInflight bounds requests one side serves concurrently; beyond it
	// a request is refused with CodeOverloaded rather than blocking the
	// reader (which also carries the responses nested callbacks wait for).
	MaxInflight = 64
	// InTransactionDeadlineMillis is the hard deadline for methods the host
	// calls while holding a coordination transaction (Method.InTransaction).
	InTransactionDeadlineMillis = 250
)
