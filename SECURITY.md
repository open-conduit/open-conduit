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

Open Conduit is a single-user local daemon. It trusts the operator's account and the operator's configuration. The [architecture documents](https://open-conduit.github.io/open-conduit/architecture/) record the details; this is a summary.

**Local management API.** The daemon serves its management API only on a Unix socket in the state directory. It never listens on TCP for management. The socket is `0600`. The config, state and private directories must be owner-only (`0700`), and config, secret, credential and database files `0600`. The daemon refuses unsafe ownership, group or world access and symlinked private paths instead of trusting them. Every endpoint requires a bearer token. The operator token is created under the daemon lock in `private/operator.token`, and the daemon stores only its SHA-256 digest. Tokens can be rotated and revoked. See [configuration and local management](https://open-conduit.github.io/open-conduit/architecture/config-management/) and [toolbelt authority](https://open-conduit.github.io/open-conduit/architecture/toolbelt-management/).

**Execution tokens.** Each execution attempt gets its own toolbelt token: 32 random bytes, stored only as a SHA-256 digest. A newer attempt's token revokes older ones, and a token cannot outlive the attempt's deadline. An execution token works only for toolbelt calls on its own execution's socket: the management socket refuses execution tokens on every route, toolbelt calls included (`403 wrong_credential_class`). Each execution's toolbelt is served on a socket of its own, `toolbelt.sock` in its run directory (`runs/<key>/` in the state directory, owner-only), which answers only toolbelt calls and only that execution's token: operator endpoints are not served there and operator credentials are refused. Its current token is kept in a `token/` directory beside it, written then renamed, and replaced there when a restarted daemon adopts the attempt. The run directory goes away once the attempt is terminal. Every call is checked against the operations the binding allows, the scope's current authorization ceiling (so a config change narrows a live token at once) and the attempt's lease. Operator credentials never reach the toolbelt endpoint.

**Secrets.** The config holds only secret references (`keyring:`, `file:`, `env:` or `cmd:`), never values. Literals in secret slots are rejected. `file:` names a key in an owner-only `secrets.json`, not an arbitrary path. A `cmd:` reference runs `sh -c` as the operator with a deadline, bounded output and a minimal environment, so the config file is trusted executable input. Validation errors, API responses, doctor output and telemetry do not echo secret values or reference text, and telemetry redacts known secret values. Secrets are resolved only when needed, and each goes only to the binding or extension entry that selects it.

**Executions and agents.** Shell and Claude Code executions run as the operator, in the configured workspace or directory. Unless their binding declares a sandbox (below), they can run any code, use the network and read whatever the operator's account can read. That includes `private/operator.token`, which gives operator access to the management socket, and other executions' run directories: an execution's own toolbelt socket narrows what its credential reaches, not what its process can read. The Claude Code executor defaults to `bypassPermissions` so that it can run unattended. An execution receives a sanitized environment plus the secrets its binding selects, so it can read those secrets and could write them to its own output. Agents act on issue, pull request and message text from the connected services. Treat that text as untrusted input that can try to steer the agent, and give bindings only the secrets, operations and repositories they need. Workspaces never embed a token in a remote or the environment, and URL credentials are redacted from logged git output. See [workspaces](https://open-conduit.github.io/open-conduit/architecture/workspaces/), [the shell executor](https://open-conduit.github.io/open-conduit/architecture/shell-executor/) and [the Claude Code executor](https://open-conduit.github.io/open-conduit/architecture/claude-code-executor/).

**The operator token and unsandboxed executions.** An unsandboxed execution can take operator access to the daemon: it can read `private/operator.token` and open `management.sock`, or run `open-conduit` itself. An agent steered by injected issue or pull request text can therefore change the config, cancel Work or do anything else the operator can. This is a known residual risk, not a bug, because nothing in Open Conduit can stop a process that runs as the same OS user from doing what that user can:

- **Owner-only files and sockets** (`0700`, `0600`) keep out other users, not the operator's own processes.
- **The macOS Keychain** trusts an application, not a caller, and the agent can run the trusted `open-conduit` binary. The **Linux Secret Service** hands every item to any process on the user's session once the collection is unlocked.
- **A token held only in memory** still has to reach the short-lived CLI from somewhere the agent can reach too, and a same-user process can often read another's memory.
- **Peer-credential checks on the socket** see the same uid. Telling an execution apart by process ancestry fails to `setsid`, a double fork or pid reuse.

The mitigation is a kernel boundary: a binding's process sandbox keeps `private/` and `management.sock` out of reach whatever its policy says. Running executions as a separate OS user also closes it, at the cost of workspace permissions and separate agent and git credentials. Treat every unsandboxed binding as holding the operator's access.

**Sandboxed bindings.** A binding with a `sandbox` runs its executor's process inside a local process sandbox: srt, which is Seatbelt on macOS and bubblewrap with seccomp on Linux, with an allowlisting network proxy. What it trusts and what it guarantees:

- **The operator's policy is the grant.** A run gets exactly the paths, hosts and sockets the binding's policy lists. An executor's or a workspace's needs are checked against it and never widen it. A need outside it fails the run before anything starts. The network is denied unless a host is listed.
- **The host protects itself whatever the policy says.** The state directory (the operator token, `management.sock`, the database, other executions' run directories, tokens and workspaces), the fallback toolbelt socket root and the config file are denied. Inside them a run may write only its own run directory and workspace, and read only its own toolbelt token and socket and the published playbooks. A policy that would reach into them is refused. Paths are compared after resolving symlinks and, on Linux, bind mounts. A workspace holding a hard link to a file outside it is refused, since no path policy can tell a hard link from its original.
- **Fail closed.** A sandbox that can't be enforced runs nothing: srt missing or too old, an unsupported platform, a kernel that won't make namespaces, srt warning that it would degrade, or a policy it can't express.
- **Git workspaces.** A sandboxed binding takes only a separate `clone`, never a worktree of the operator's checkout, and no `setup`, which would run outside the sandbox. The clone's `.git/config`, hooks and `info` are read-only inside it, so the agent can't plant something for the host's own git to run.
- **No attach.** `open-conduit work attach` runs the executor's session on the host, outside the sandbox, and host-side Claude Code loads hooks and MCP servers from the workspace the agent could write. Attach is refused for Work that ran in a sandbox.
- **Claude Code's config.** A sandboxed Claude Code binding keeps its config in a directory of its own (`CLAUDE_CONFIG_DIR` in its `env`), and the operator's `~/.claude` and `~/.claude.json` are neither readable nor writable inside it, so the agent can't plant a hook, MCP server or plugin for the operator's own claude sessions to run. A binding without one is refused.
- **Pinned and proved.** Each run's policy is recorded and pinned with its segment before it starts. A restarted daemon adopts a running segment only after proving it is still the sandbox it launched, not just that the policy file matches. Otherwise the segment is stopped.

Residual risks: srt itself, its proxy and the provider run as the operator and are trusted. A run can read everything its policy doesn't deny, so deny your credentials (`~/.ssh`, `~/.aws`, `~/.config/gh`). Every host it may reach can receive what it can read, including the secrets its binding selects. On Linux, `"unix_sockets": "all"` makes every unix socket it can see reachable (the known container sockets are hidden). The hard-link check covers the workspace only. Claude Code's own sandbox setting is defence in depth inside srt, not a substitute. See [process sandboxes](https://open-conduit.github.io/open-conduit/architecture/sandboxes/).

**Container bindings.** A binding with a `container` runs its executor in a Docker container, under the same rules as a sandbox: the binding's config is the grant, the host's protected set always applies, and anything that can't be enforced runs nothing. Nothing of the host is visible unless granted, and the container runs as the operator with no capabilities and a read-only root. It has no route out: it reaches only its gateway container, whose proxy allows only the grant's `network.allow` and refuses an allowed name that resolves to a loopback, link-local, private or host address, and its own toolbelt through the gateway's forward. On macOS the host serves that toolbelt on a loopback TCP port for the execution, authorised by the execution's token alone, since a host socket can't be reached from Docker's VM: any local process, and any container with a route to the host, can reach that port. The gateway image and the provider run as the operator and are trusted; the gateway doesn't inspect TLS, so an allowed host can receive anything the container can read. Docker Desktop is unverified. See [containers](https://open-conduit.github.io/open-conduit/architecture/sandboxes/#containers).

**Extensions.** An installed extension package runs as a subprocess with the operator's privileges. Process separation is fault isolation, not a sandbox: the extension can read and write the operator's files, open network connections and start processes. Installing one is the same decision as installing any program you run. The host enforces only the protocol surface: scoped storage namespaces, scoped state reads, declared event families, causal limits, and only the secrets its own entries select. Certification checks protocol conformance and does not make a malicious extension safe. Packages are not signed. See [the extension protocol](https://open-conduit.github.io/open-conduit/architecture/extension-protocol/#trust-model) and [extension packaging](https://open-conduit.github.io/open-conduit/architecture/extension-packaging/).

**Web view.** `open-conduit web` is a separate process, a client of the management socket like the TUI. `serve --web` runs the same view inside the daemon's process, still as a client of the socket. Either way the daemon never listens on TCP for management. The web view holds the operator token in its own process and never sends it to the browser. It binds only loopback addresses, and refuses any request whose `Host` is not its configured name, `localhost` or its address, which defeats DNS rebinding. A browser gets in only through a random sign-in URL, which sets a host-only, `HttpOnly`, `SameSite=Strict` session cookie. Each sign-in mints a new session, signed with a session key, so a cookie planted before sign-in is never the one that gets in. A session lapses after 12 hours idle or 7 days in all. **Sign out everywhere** in the view replaces the key, which ends every session. The sign-in token and the session key are kept in `private/web.signin` in the state directory (`0600`), next to the operator token, so they last across restarts. Cookies are not kept apart by port, so the second sign-in URL, which names `127.0.0.1`, leaves the session readable by any other server on that address (an agent's dev server, say); the view prints a warning beside it, and the first URL, on `open-conduit.localhost`, keeps the cookie to that name. `open-conduit web url` prints the URL. Sandboxed agents can't read the file, because the whole state directory is protected from them. Every change needs a per-process CSRF token and a same-origin `Origin`. Pages are served with a `default-src 'self'` Content Security Policy and `Referrer-Policy: same-origin`, so no URL leaves for another site; `no-referrer` would make browsers send `Origin: null` with the view's own forms, which the origin check refuses. Anyone who can read that URL, the file or the log it was printed to can sign in as the operator until the file is deleted. That includes the update page, which installs the latest release over the running binaries; like `open-conduit upgrade`, it installs only an archive that matches the release's `checksums.txt`, downloaded over HTTPS from GitHub, and it leaves a Homebrew install to `brew`. To show that a newer release is out, the view asks GitHub's API for the latest release at most every six hours.

**Webhooks.** The webhook listener is off unless the config stores `settings.webhooks` or `serve --webhook-addr` is given; without it the daemon only reconciles. The listener is a separate HTTP server that never serves the management API or sees its credentials. It mounts a route only for entries that name a signing secret, and it verifies each delivery before parsing it. An empty secret never verifies.

- GitHub: `X-Hub-Signature-256` HMAC-SHA256, compared in constant time.
- Slack: `X-Slack-Signature` HMAC-SHA256 over the timestamp and body, with a 5-minute replay window.
- Linear: `Linear-Signature` HMAC-SHA256 with a 10-minute replay window.
- Jira and Bitbucket: `X-Hub-Signature` HMAC-SHA256. Unsigned deliveries are refused.
- Scheduled job webhooks: a per-job URL token derived from the entry's webhook secret, compared in constant time. Rotating the secret revokes the URLs.
- Installed extension packages: the host forwards the request to the package, which must verify it itself.

A bad signature gets 401 (404 for a job URL) and changes nothing. Reconcile recovers anything a refused delivery would have reported. See [GitHub](https://open-conduit.github.io/open-conduit/architecture/github-extension/), [Slack](https://open-conduit.github.io/open-conduit/architecture/slack-extension/), [installed integrations](https://open-conduit.github.io/open-conduit/architecture/installed-integrations/), [scheduled jobs](https://open-conduit.github.io/open-conduit/architecture/schedule-extension/) and [operator setup](https://open-conduit.github.io/open-conduit/architecture/operator-setup/#webhooks-tunnels-and-reconcile-only-operation).
