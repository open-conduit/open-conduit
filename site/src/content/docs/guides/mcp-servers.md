---
title: Add tools from MCP servers
description: Give every agent the tools of an MCP server, local or remote, as toolbelt operations, with the server sandboxed, its secrets kept from the model and every call in run history.
---

An MCP server is the quickest way to give agents a tool of your own, such as looking up a customer in your CRM, without writing an extension. The `mcp` package runs the server, or reaches it over HTTP, and turns each tool you list into a toolbelt operation such as `mcp.crm.lookup_customer`. Every executor that reaches the toolbelt gets it: Claude Code, Codex, the [Chat Completions executor](../chat-completions/) and shell scripts. Grants, gates and run history apply as they do to any other operation, and the server's secrets reach the server, never the model.

[The MCP bridge](../../architecture/mcp-bridge/) is the design record.

## What you need

- **The package.** Homebrew, the release archives and the install script bundle it in `extensions/mcp/`. Check it's there with `open-conduit extension list`. A source build brings no packages: run `make packages` and then `open-conduit extension install dist/packages/mcp` ([install](/open-conduit/start/install/#from-source)).
- **For a local server, a sandbox.** A server you run as a command runs in a process sandbox. Set up `srt` as [Sandbox your agents](../sandboxes/) describes; `open-conduit doctor` says whether it can isolate here.
- **The server's tool list.** You list each tool you take, with its description and input schema exactly as the server gives them. The [MCP Inspector](https://modelcontextprotocol.io/docs/tools/inspector) (`npx @modelcontextprotocol/inspector`) shows them: connect it to the server, choose **List Tools**, and copy each tool's `description` and `inputSchema`.

## A local server

Store the server's credentials as a secret:

```sh
open-conduit secret set crm_token
```

Name the secret in the scope's `secret_refs`, with the reference `secret set` printed, and add an entry to the scope's `extensions`:

```json
{
  "name": "crm",
  "type": "mcp-stdio",
  "secret_names": ["crm_token"],
  "config": {
    "command": ["npx", "-y", "@acme/crm-mcp"],
    "secret_env": { "CRM_TOKEN": "crm_token" },
    "tools": [
      {
        "name": "lookup_customer",
        "description": "Look up a customer by ID. Returns their name, plan and account owner.",
        "input_schema": {
          "type": "object",
          "properties": { "id": { "type": "string" } },
          "required": ["id"]
        },
        "retry_safe": true
      }
    ]
  },
  "sandbox": {
    "provider": "srt",
    "config": {
      "write": ["~/.npm"],
      "network": { "allow": ["registry.npmjs.org", "api.acme-crm.com"] }
    }
  }
}
```

- **`command`** is the server's argv. It runs in a fresh directory of its own, which the policy's `{workspace}` and `{run}` both name and which is always writable.
- **`secret_env`** sets a variable from a secret the entry selects. `env` sets plain ones. The server also sees `PATH`, `HOME`, `LANG` and `LC_ALL`, and nothing else of the daemon's environment.
- **`sandbox`** is the policy, as a binding's is ([the policy](../sandboxes/#process-sandboxes)). Allow only the hosts the server needs: here npm, to fetch the package, and the CRM's API. A server that tries to reach anything else is refused by srt's proxy.

The server starts on the first call and is kept running for later ones. It is restarted when you change its entry and stopped when you remove it.

## A remote server

A server you reach over HTTP needs no sandbox, since nothing runs on your machine:

```json
{
  "name": "crm",
  "type": "mcp-http",
  "secret_names": ["crm_token"],
  "config": {
    "url": "https://mcp.acme-crm.com/mcp",
    "bearer_secret": "crm_token",
    "tools": [ … ]
  }
}
```

`bearer_secret` sends a secret as `Authorization: Bearer …`. A server that wants another header takes `secret_headers`, a map from header name to secret name, and plain ones take `headers`. The URL must be `https`.

## Grant the tools

Each tool is the operation `mcp.<entry>.<operation>`: here `mcp.crm.lookup_customer`. Grant it by name, or grant every tool of the entry with the toolset `@mcp.crm.tools`, in the scope's `authorization.operations` and the binding's `operations`:

```json
"operations": ["read_context", "wait", "declare_outcome", "@mcp.crm.tools"]
```

`open-conduit config check` resolves the grant from the entry's tool list without starting the server.

A tool whose server name isn't a valid operation name, such as `lookupCustomer`, takes `"operation": "lookup_customer"`. A tool can join a toolset of its own with `"toolset": "read"`, granted as `@mcp.crm.read`, instead of `tools`.

## When the server changes a tool

The tool's description and schema in your config are what you approved. Before every call, the bridge checks that the server still offers the tool with exactly that description and schema. If the server has rewritten the description, changed the schema or dropped the tool, the call is refused as `drifted` and the server isn't called. That matters because a description is text the agent reads as instructions.

To accept the change, look at the new description and schema in the Inspector, copy them into the entry, and apply the config. A tool the server adds is never offered until you list it.

## Retries

A call can be cut off after the server got it and before its answer was recorded. By default the bridge never sends such a call again: a retry with the same request is refused as `unknown_outcome`, and the agent is told to check before asking again. A retry that comes after the answer was recorded gets the same answer without reaching the server. Mark a tool `"retry_safe": true` when sending it twice does no harm, such as a lookup or a search, and it is sent every time.

## If you load MCP servers into Claude Code

Unless a binding sets `strict_mcp`, Claude Code also loads the MCP servers in your own Claude settings. Those calls don't go through the toolbelt: Open Conduit doesn't see them, check them or record them, and the server's credentials sit in the agent's config. Move the server to an `mcp-stdio` or `mcp-http` entry instead, and set `"strict_mcp": true` on the binding so it loads only the toolbelt.

## When a call fails

| Error | What to do |
| --- | --- |
| `drifted` | Review the server's change and re-apply it ([above](#when-the-server-changes-a-tool)). |
| `sandbox_refused` | The policy refused the launch: the message names what. Check `open-conduit doctor` and the policy. |
| `server_unavailable` | The server didn't start or answer. Run its command, or reach its URL, by hand to see why. A secret it needs may have no value in the scope. |
| `tool_error`, `tool_failed` | The tool or the server refused the call. The message is theirs. |
