---
title: "The MCP bridge"
description: "How the mcp package turns the tools an operator lists from MCP servers into toolbelt operations: entry operations, the pinned description and schema, retries, the sandboxed stdio launch and remote servers."
---

:::tip
To add an MCP server's tools, see [Add tools from MCP servers](../../guides/mcp-servers/). This page is the design record.
:::

The MCP bridge lets an operator give agents their own tools without writing an extension. An entry names an MCP server, either a command or a remote URL, and lists the tools to take from it. Each tool becomes a toolbelt operation such as `mcp.crm.lookup_customer`, with the input schema the server gives it. Every executor that reaches the toolbelt gets these tools, whether it is Claude Code, Codex, the [Chat Completions executor](../chat-completions-executor/) or a shell script. Authority, secrets, gates and the trail stay at the host.

The alternative, an operator's MCP servers loaded straight into Claude Code (as a binding without `strict_mcp` does), bypasses all of that. Those calls are invisible to Open Conduit and unchecked by it, the server's secrets sit in the agent's config, and other executors don't get the tools.

The bridge is the bundled package [`extensions/mcp`](https://github.com/open-conduit/open-conduit/tree/main/extensions/mcp), served as `open-conduit extension-run mcp`. It is built on two public contracts any installed package can use: [entry operations](../extension-protocol/#entry-operations) and [entry sandboxes](../sandboxes/#entry-processes). Nothing in the core knows about MCP.

## Entry types

| Type | Server | Sandbox |
| --- | --- | --- |
| `mcp-stdio` | `command`, run as a local process speaking MCP over stdin and stdout | required |
| `mcp-http` | `url`, a remote server speaking MCP's streamable HTTP transport | not allowed: nothing runs locally |

The type declares its sandbox mode in the manifest (`extensions[].sandbox`), so config check refuses an `mcp-stdio` entry without a sandbox (`sandbox_required`) and an `mcp-http` entry with one (`unsupported_sandbox`).

Both take `tools`, the list of tools taken from the server, and `timeout_seconds` (default 60), which bounds a call and the server's start. `mcp-stdio` takes `env` and `secret_env`, a map from variable to secret name. `mcp-http` takes `headers`, `secret_headers` (header to secret name) and `bearer_secret`, a secret sent as the bearer token. Every secret named must be one the entry selects in `secret_names`. `url` must be `https`, or `http` to a loopback address, since the server may be sent the entry's secrets.

## Tools become operations

Each tool in `tools` is:

| Field | Meaning |
| --- | --- |
| `name` | The server's name for the tool. |
| `operation` | The operation's name within the entry, default `name`. Needed when the server's name isn't a name segment (a lower-case letter, then letters, digits, `-` or `_`), such as `lookupCustomer`. |
| `description` | The tool's description, exactly as the server lists it. |
| `input_schema` | The tool's `inputSchema`, exactly as the server lists it. |
| `summary` | The operation's one-line summary. Default: the description's first line, cut to 200 bytes. |
| `toolset` | The toolset within the entry the operation joins, default `tools`. |
| `retry_safe` | Whether a call may be sent again when its outcome wasn't recorded. Default `false`. |

The package answers `validate` with one [entry operation](../extension-protocol/#entry-operations) per tool, read from the config alone, so `config check` resolves grants without starting a server. The operation is `mcp.<entry>.<operation>`, with the description as its guidance and the pinned schema as its input. `@mcp.<entry>.tools` grants every tool of an entry that keeps the default toolset.

The operator lists the tools. A tool the server offers but the entry doesn't list is never an operation, so a server that adds a tool grants nothing.

## Pin what was approved

The entry's config is the record of what the operator approved: each tool's description and input schema. A description is prompt text the agent reads as guidance, so a server that rewrites it quietly could steer the agent. The bridge therefore lists the server's tools before every call and compares the tool with the pinned one. The description must be identical, and the schema must be the same JSON (key order aside). A tool whose description or schema has changed, or that the server no longer offers, is refused as `drifted` without being called. The refusal lasts until the operator reviews the change and re-applies the entry's config with the new description or schema.

The bridge lists before each call rather than caching the list, so a change is caught on the next call whether or not the server sends `notifications/tools/list_changed`. The cost is one extra round trip per call.

## Retries

The toolbelt replays a committed result for a retried `request_id` within an execution, but it can't make the downstream server idempotent. A call cut off after it reached the server and before its result was recorded would be sent again. So every tool declares whether that is safe, and by default it isn't.

For a tool without `retry_safe`, the bridge marks the request in its KV (`mcp.calls`, keyed by scope, entry, operation and request ID) before sending it, and records the outcome once it has one:

- The same request again, once its outcome is recorded, gets the same answer without reaching the server. A result larger than 256 KiB isn't kept, so its repeat is refused as `already_done`.
- The same request again while the mark has no outcome, because the call went out and nothing came back, is refused as `unknown_outcome`. The agent is told to check the system the tool acts on before asking again with a new request.
- A call with no request ID isn't marked.

A tool marked `retry_safe` is sent every time. A task, `mcp-sweep-calls`, deletes marks older than seven days every hour.

## The stdio server and its sandbox

An `mcp-stdio` server is user-supplied code running on the operator's machine, so it runs in the sandbox its entry names, through the host. The bridge asks for the launch with `sandbox.entry_plan`, giving the entry's scope and name, the command and the environment. The host plans it with the provider and policy from the entry's own `sandbox` (an `srt` policy, say), never one the package chooses, with its protected set denied. It makes a fresh directory for the process beneath `<state>/entries/mcp/runs/`, which is both the process's working directory and its run directory ([entry processes](../sandboxes/#entry-processes)). The bridge runs exactly the plan's argv and environment there, in a process group of its own.

The server's environment is `PATH`, `HOME`, `LANG` and `LC_ALL` from the daemon, the entry's `env` and its `secret_env` resolved from the scope's secrets, and nothing else of the daemon's. Secrets reach the server, never the model or the agent's process. Network access is whatever the sandbox policy allows: srt denies every host its `network.allow` doesn't name.

The bridge keeps one server per entry in each scope. It starts the server on the first call and keeps it for later ones. A server whose entry's config has changed is stopped and started again on the next call, and a server whose entry is gone is stopped when the package is configured. Stopping closes the server's stdin and kills its process group if it hasn't exited within two seconds. The server's directory is removed when it exits. The server's stderr isn't kept, since it may print what the server was given. A restarted daemon clears `<state>/entries`, and the bridge starts servers afresh.

The bridge answers requests the server sends it: `ping`, and a method-not-found error for anything else (sampling, elicitation, roots), since it offers the server no client capabilities.

## Remote servers

An `mcp-http` entry's server is reached over MCP's streamable HTTP transport. Each message is a POST with `Accept: application/json, text/event-stream`. A response may be JSON or an event stream, and the bridge reads the stream until the response it is waiting for arrives. The session ID the server assigns at `initialize` (`Mcp-Session-Id`) and the negotiated `MCP-Protocol-Version` go with every later request, and the session is ended with a DELETE when the bridge stops using it. The bridge opens no GET stream, because it needs no server notifications: it lists tools before each call. An error never includes the URL or the headers.

## What a call returns

A successful call's output has schema `mcp.result.v1`: `content`, the tool's content blocks as the server returned them, and `structured_content` when it gave any. Refusals are operation errors with these codes:

| Code | Meaning |
| --- | --- |
| `undeclared` | The entry's config doesn't declare the operation. |
| `drifted` | The server no longer offers the tool as pinned. |
| `tool_error` | The tool reported an error (`isError`); the message is its text. |
| `tool_failed` | The server refused the call with a JSON-RPC error. |
| `server_unavailable` | The server didn't start, list its tools or answer. A call it didn't answer may or may not have run. |
| `sandbox_refused` | The sandbox refused the launch (`sandbox_denied` or `sandbox_unavailable`), and nothing ran. |
| `unknown_outcome`, `already_done` | A repeat of a call that isn't safe to retry ([retries](#retries)). |

## Trail

Calls are ordinary toolbelt operations, so each appears in run history as a `toolbelt.*` record linked to its Work and Execution, with its grant check, any gate's decision and its outcome, refusals included. The host logs each sandboxed launch with the entry, provider, plan digest and summary.

## Limitations

- **Only process sandboxes.** An `mcp-stdio` server runs in a process sandbox provider such as `srt`. Entry processes can't yet run in a container provider such as Docker.
- **Pinning is by hand.** The operator copies each tool's description and input schema from the server into the config, for example from the MCP Inspector, and copies them again to approve a change. There is no command that fetches and pins them yet.
- **Tools only.** MCP resources and prompts aren't bridged, and a server's requests to the client (sampling, elicitation, roots) are refused.
- **A description is guidance.** An operation's guidance is at most 4096 bytes, so a tool whose description is longer is refused at config check.

## Evidence

- `extensions/mcp` tests run a fake MCP server, the test binary itself, over stdio and an `httptest` server over streamable HTTP. They cover listing across pages, a call with content and structured content, a tool the server offers but the entry doesn't list (`undeclared`, never sent), a rewritten description, a changed schema and a withdrawn tool (`drifted`, never sent, then accepted once re-applied), a reordered schema (no change), the sandboxed launch (the host is asked for the entry's own launch, the server runs in the directory it made, inside its wrapper, with the entry's secret and none of the daemon's other variables), a refused sandbox (nothing runs), retries (a repeat answered from the mark, an unknown outcome refused, a retry-safe tool sent again, marks swept), the bearer token and session on HTTP, and configure stopping a dropped entry's server and removing its directory.
- `internal/bootstrap` tests the host's side of the launch: the entry's own provider and policy, a fresh directory beneath the package's runs, the state directory denied, and refusal for an entry without a sandbox, an entry of another package's type, or another scope.
- `TestMCPBridgeThroughTheDaemon` runs the bundled package in the real daemon with a test sandbox provider. An agent's call reaches the server inside the sandbox, with the operator's policy, in the host's directory and with the entry's secret. A tool the entry doesn't list isn't callable, and a remote tool whose description has changed is refused as drifted.

## History

:::note
These notes record how this part was built. They describe the code at the time; the sections above describe current behaviour.
:::

- **#660** asked for a Chat Completions executor that runs toolbelt-only Work, with user-supplied tools. Its design made every tool a toolbelt operation and proposed the MCP bridge on a new contract for operations that config declares.
- **#663** added that contract, [entry operations](../extension-protocol/#entry-operations).
- **#664** added the bridge and [entry sandboxes](../sandboxes/#entry-processes), so its stdio servers run in the sandbox their entry names.
