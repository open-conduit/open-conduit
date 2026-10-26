# Security policy

## Supported versions

Security fixes go to the latest release and the `main` branch. Older releases do not get backports.

Open Supervisor is a separate project with its own policy. Report its issues there.

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability reporting on this repository:

1. Open <https://github.com/open-conduit/open-conduit/security/advisories/new> (the **Security** tab, then **Report a vulnerability**).
2. Describe the issue, the affected commit or version, your platform, and the steps to reproduce it.

Do not open a public issue, pull request or discussion for a suspected vulnerability. Leave real tokens, secret values and private repository content out of the report. A redacted log or a synthetic reproduction is enough.

## What to expect

- The maintainer aims to acknowledge a report within 7 days.
- The maintainer confirms or rejects the issue in the advisory thread and keeps you updated while a fix is prepared.
- Fixed issues are disclosed through a GitHub security advisory once a fix is on `main` (or in a release, once releases exist). You are credited unless you ask not to be.

## Scope

In scope is the code in this repository:

- the `open-conduit` daemon and operator CLI, and the `os` toolbelt CLI;
- the local management API, operator and execution credentials, and toolbelt authority checks;
- configuration loading, secret reference resolution and redaction;
- the webhook listener and signature verification;
- the extension host's enforcement of the subprocess protocol, and the extension install commands;
- the first-party extensions, executors and packages;
- the Open Supervisor configuration converter.

Out of scope:

- behavior the trust model below accepts, such as an installed extension or an agent acting with the operator's privileges, or a `cmd:` secret reference running its command;
- anything that needs prior access to the operator's account or to its owner-only files;
- vulnerabilities in GitHub, Slack, Linear, Jira, Bitbucket, PostHog, Claude Code, tunnel providers or other third-party services and tools (report those to their vendors);
- third-party extension packages (report those to their authors).

## Trust model

Open Conduit is a single-user local daemon. It trusts the operator's account and the operator's configuration. The [architecture documents](docs/README.md) record the details; this is a summary.

**Local management API.** The daemon serves its management API only on a Unix socket in the state directory. It never listens on TCP for management. The socket is `0600`. The config, state and private directories must be owner-only (`0700`), and config, secret, credential and database files `0600`. The daemon refuses unsafe ownership, group or world access and symlinked private paths instead of trusting them. Every endpoint requires a bearer token. The operator token is created under the daemon lock in `private/operator.token`, and the daemon stores only its SHA-256 digest. Tokens can be rotated and revoked. See [configuration and local management](docs/architecture/config-management.md) and [toolbelt authority](docs/architecture/toolbelt-management.md).

**Execution tokens.** Each execution attempt gets its own toolbelt token: 32 random bytes, stored only as a SHA-256 digest. A newer attempt's token revokes older ones, and a token cannot outlive the attempt's deadline. An execution token works only for toolbelt calls, never for operator endpoints. Every call is checked against the operations the binding allows, the scope's current authorization ceiling (so a config change narrows a live token at once) and the attempt's lease. Operator credentials never reach the toolbelt endpoint.

**Secrets.** The config holds only secret references (`keyring:`, `file:`, `env:` or `cmd:`), never values. Literals in secret slots are rejected. `file:` names a key in an owner-only `secrets.json`, not an arbitrary path. A `cmd:` reference runs `sh -c` as the operator with a deadline, bounded output and a minimal environment, so the config file is trusted executable input. Validation errors, API responses, doctor output and telemetry do not echo secret values or reference text, and telemetry redacts known secret values. Secrets are resolved only when needed, and each goes only to the binding or extension entry that selects it.

**Executions and agents.** Shell and Claude Code executions run as the operator, in the configured workspace or directory. They are not sandboxed. They can run any code, use the network and read whatever the operator's account can read. The Claude Code executor defaults to `bypassPermissions` so that it can run unattended. An execution receives a sanitized environment plus the secrets its binding selects, so it can read those secrets and could write them to its own output. Agents act on issue, pull request and message text from the connected services. Treat that text as untrusted input that can try to steer the agent, and give bindings only the secrets, operations and repositories they need. Workspaces never embed a token in a remote or the environment, and URL credentials are redacted from logged git output. See [workspaces](docs/architecture/workspaces.md), [the shell executor](docs/architecture/shell-executor.md) and [the Claude Code executor](docs/architecture/claude-code-executor.md).

**Extensions.** An installed extension package runs as a subprocess with the operator's privileges. Process separation is fault isolation, not a sandbox: the extension can read and write the operator's files, open network connections and start processes. Installing one is the same decision as installing any program you run. The host enforces only the protocol surface: scoped storage namespaces, scoped state reads, declared event families, causal limits, and only the secrets its own entries select. Certification checks protocol conformance and does not make a malicious extension safe. Packages are not signed. See [the extension protocol](docs/architecture/extension-protocol.md#trust-model) and [extension packaging](docs/architecture/extension-packaging.md).

**Webhooks.** The webhook listener is off unless the config stores `settings.webhooks` or `serve --webhook-addr` is given; without it the daemon only reconciles. The listener is a separate HTTP server that never serves the management API or sees its credentials. It mounts a route only for entries that name a signing secret, and it verifies each delivery before parsing it. An empty secret never verifies.

- GitHub: `X-Hub-Signature-256` HMAC-SHA256, compared in constant time.
- Slack: `X-Slack-Signature` HMAC-SHA256 over the timestamp and body, with a 5-minute replay window.
- Linear: `Linear-Signature` HMAC-SHA256 with a 10-minute replay window.
- Jira and Bitbucket: `X-Hub-Signature` HMAC-SHA256. Unsigned deliveries are refused.
- Scheduled job webhooks: a per-job URL token derived from the entry's webhook secret, compared in constant time. Rotating the secret revokes the URLs.
- Installed extension packages: the host forwards the request to the package, which must verify it itself.

A bad signature gets 401 (404 for a job URL) and changes nothing. Reconcile recovers anything a refused delivery would have reported. See [GitHub](docs/architecture/github-extension.md), [Slack](docs/architecture/slack-extension.md), [installed integrations](docs/architecture/installed-integrations.md), [scheduled jobs](docs/architecture/schedule-extension.md) and [operator setup](docs/architecture/operator-setup.md#webhooks-tunnels-and-reconcile-only-operation).
