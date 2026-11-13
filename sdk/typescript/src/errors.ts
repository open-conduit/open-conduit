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
  NotLive: "not_live",
  ResourceSetup: "resource_setup_failed",
  ResourceNotFound: "resource_not_found",
  ResourceExhausted: "resource_exhausted",
  SandboxDenied: "sandbox_denied",
  SandboxUnavailable: "sandbox_unavailable",
  ContainerRunning: "container_running",
  ContainerUnconfirmed: "container_unconfirmed",
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
  /** A config refusal's diagnostics (ConfigError), as they crossed. */
  readonly diagnostics: ConfigDiagnostic[];
  constructor(
    code: string,
    detail = "",
    resetAt?: string,
    diagnostics: ConfigDiagnostic[] = [],
  ) {
    super(detail === "" ? `protocol: ${code}` : `protocol: ${code}: ${detail}`);
    this.name = "ProtocolError";
    this.code = code;
    this.detail = detail;
    this.resetAt = resetAt;
    this.diagnostics = diagnostics;
  }
}

/**
 * One thing wrong with a config object (extension.ConfigDiagnostic): the
 * JSON pointer of the field within the object ("/jobs/1/cron", "" for the
 * object itself), a stable snake_case code and a message saying what is
 * wrong and what is allowed. The host shows it to the operator, so the
 * message must never quote a configured value.
 */
export interface ConfigDiagnostic {
  path: string;
  code: string;
  message: string;
}

/**
 * A refusal of a config object that says why: throw one from an entry
 * type's validate hook and the host reports each diagnostic at its field's
 * full path. Anything else thrown is reported only as invalid.
 */
export class ConfigError extends Error {
  readonly diagnostics: ConfigDiagnostic[];
  constructor(diagnostics: ConfigDiagnostic[]);
  constructor(path: string, code: string, message: string);
  constructor(
    first: ConfigDiagnostic[] | string,
    code?: string,
    message?: string,
  ) {
    const diagnostics =
      typeof first === "string"
        ? [{ path: first, code: code ?? "", message: message ?? "" }]
        : first;
    super(
      "invalid config: " +
        diagnostics
          .map((d) => `${d.path || "config"}: ${d.code}: ${d.message}`)
          .join("; "),
    );
    this.name = "ConfigError";
    this.diagnostics = diagnostics;
  }
}

/** The JSON pointer of a path of object keys and array indexes. */
export function pointer(...parts: (string | number)[]): string {
  return parts
    .map((p) => "/" + String(p).replaceAll("~", "~0").replaceAll("/", "~1"))
    .join("");
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
export function decode(
  code: string,
  message: string,
  resetAt?: string,
  diagnostics: ConfigDiagnostic[] = [],
): Error {
  return isProtocolCode(code)
    ? new ProtocolError(
        code,
        message,
        resetAt,
        code === Code.Invalid ? diagnostics : [],
      )
    : new OperationError(code, message);
}

/** The code and message an error crosses the wire with. */
export function encode(err: unknown): {
  code: string;
  message: string;
  resetAt?: string;
  diagnostics?: ConfigDiagnostic[];
} {
  if (err instanceof ConfigError && err.diagnostics.length > 0)
    return {
      code: Code.Invalid,
      message: "invalid config",
      diagnostics: err.diagnostics,
    };
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
