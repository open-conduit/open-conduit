// Errors that cross the wire, as protocol/errors.go defines them: an
// authored error is a stable code and a short, secret-free message, sent as
// JSON-RPC code -32000 with data.code. Anything else crosses as "internal"
// with no detail, which stays in the sender's own log (stderr).

/** The protocol's own error codes (protocol/protocol.go). */
export const Code = {
  UnsupportedVersion: "unsupported_protocol_version",
  MethodNotFound: "method_not_found",
  Invalid: "invalid",
  Unsupported: "unsupported",
  Unauthorized: "unauthorized",
  Undeclared: "undeclared",
  NotFound: "not_found",
  Conflict: "conflict",
  CausalLimit: "causal_limit",
  NotAdoptable: "not_adoptable",
  NotAccepting: "not_accepting",
  NothingToAttach: "nothing_to_attach",
  ResourceSetup: "resource_setup_failed",
  ResourceNotFound: "resource_not_found",
  ResourceExhausted: "resource_exhausted",
  RetryAfter: "retry_after",
  Reentrant: "reentrant_call",
  Overloaded: "overloaded",
  Cancelled: "cancelled",
  ShuttingDown: "shutting_down",
  Internal: "internal",
  Schema: "unreadable_schema",
  KVDowngrade: "kv_downgrade",
};

const known = new Set<string>(Object.values(Code));

/** Reports whether code is one of the protocol's own codes. */
export function isProtocolCode(code: string): boolean {
  return known.has(code);
}

/**
 * An authored protocol error: one of the protocol's own codes. The host
 * refuses with these (not_found from kv.get is never thrown; conflict from
 * kv.commit is), and an extension may throw one to refuse a host request.
 */
export class ProtocolError extends Error {
  readonly code: string;
  /** The message as it crossed the wire. */
  readonly detail: string;
  readonly resetAt: string | undefined;
  constructor(code: string, detail = "", resetAt?: string) {
    super(detail === "" ? `protocol: ${code}` : `protocol: ${code}: ${detail}`);
    this.name = "ProtocolError";
    this.code = code;
    this.detail = detail;
    this.resetAt = resetAt;
  }
}

/**
 * An operation's refusal, which reaches the calling agent verbatim: a code
 * of the extension's own ([a-z][a-z0-9_]*) and a message the agent reads.
 * Throw one from an operation handler; anything else thrown is "internal".
 */
export class OperationError extends Error {
  readonly code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = "OperationError";
    this.code = code;
  }
}

/** The error a request answered after its caller cancelled it. */
export class CancelledError extends Error {
  constructor() {
    super("request cancelled");
    this.name = "CancelledError";
  }
}

/** The connection to the host closed (it exited, or the stream broke). */
export class TransportError extends Error {
  constructor(reason = "transport closed") {
    super(`protocol: ${reason}`);
    this.name = "TransportError";
  }
}

/** Decode returns the error for an authored error received from the host. */
export function decode(code: string, message: string, resetAt?: string): Error {
  return isProtocolCode(code)
    ? new ProtocolError(code, message, resetAt)
    : new OperationError(code, message);
}

/** The code and message an error crosses the wire with. */
export function encode(err: unknown): {
  code: string;
  message: string;
  resetAt?: string;
} {
  if (err instanceof ProtocolError) {
    const message = err.detail === "" ? err.code : err.detail;
    return err.resetAt === undefined
      ? { code: err.code, message }
      : { code: err.code, message, resetAt: err.resetAt };
  }
  if (err instanceof OperationError)
    return { code: err.code, message: err.message };
  if (
    err instanceof CancelledError ||
    (err instanceof Error && err.name === "AbortError")
  ) {
    return { code: Code.Cancelled, message: "request cancelled" };
  }
  return { code: Code.Internal, message: "internal error" };
}
