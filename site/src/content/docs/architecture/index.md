---
title: "Architecture"
description: "How Open Conduit is built: its guarantees, the decisions behind them and the evidence for each."
---

These pages are for people working on Open Conduit itself, or anyone who wants to know exactly what it guarantees. To install and run it, start at [What Open Conduit does](../start/overview/).

## How to read these pages

Each page separates three kinds of material:

- **Current behaviour and guarantees.** The opening and the body of every page describe the code as it is now: what the part does, what it promises and its current limits. A limit stated here is a real limit today.
- **Design decisions.** Eight pages record a decision, numbered B01 to B08, with its rules and the reasons for them. Code and other pages cite these numbers. The decisions still hold unless the page says otherwise.
- **History.** Where a page has a **History** section at the end, it records how that part was built: which delivery ticket (numbered like `02-04`) added what, what each stage left out at the time, and the evidence runs made then. Those notes describe the code at the time, not current limitations.

[Performance](performance/) gathers the evidence on throughput, latency, contention and resource use, and names what has not been measured yet.

## Core

- [The core in one page](core/)
- [Package boundaries and dependency rules](packages/) (decision B01)
- [Primitive and transactional storage contracts](domain-storage/) (decision B02)
- [Ingress, executor, toolbelt and observability contracts](extensions/) (decision B03)
- [Routing, waits and recovery semantics](runtime-semantics/) (decision B04)
- [Configuration, secrets and local management](config-management/) (decision B05), with [the configuration schema](https://github.com/open-conduit/open-conduit/blob/main/docs/architecture/config.schema.json) and [its fixtures](https://github.com/open-conduit/open-conduit/tree/main/docs/architecture/examples/config)
- [Contracts, supported build targets and development checks](contracts-gate/) (decision B06)

## Runtime

- [SQLite and local artifact guarantees](sqlite-artifacts/)
- [Verified backup bundles](backup-bundles/)
- [Durable event acceptance and reconciliation](event-ingress/)
- [Durable routing, Work lifecycle and causal limits](routing-lifecycle/)
- [Execution scheduling, recovery and control](execution-scheduling/)
- [Durable timers, waits and dependency aliases](timers-waits/)
- [Toolbelt authority, configuration and local management](toolbelt-management/)
- [Shell executor and CLI toolbelt](shell-executor/)
- [Lifecycle telemetry, unified run history and doctor](observability-history/)
- [Daemon composition and the kill and restart drills](daemon-core-gate/)
- [Performance](performance/)

## Workloads and integrations

- [GitHub ingress, routing policy and projections](github-extension/)
- [Optional workspaces and the Git provider](workspaces/) (decision B07)
- [Claude Code executor and private continuity](claude-code-executor/)
- [Engineering tools, waits and playbooks](engineering-tools/)
- [Slack ingress, conversations and marked replies](slack-extension/)
- [Scheduled jobs](schedule-extension/)
- [Introspection: reporting, sweep, triage and fixes](issue-reporting/)
- [Operator setup, doctor and engineering controls](operator-setup/)
- [Installed integrations: Linear, Jira, Bitbucket, PostHog and operator tools](installed-integrations/)
- [Backup, restore and state recovery](recovery/)

## Extensions (provisional)

- [Extension subprocess protocol](extension-protocol/) (decision B08)
- [Extension packaging, SDK and certification](extension-packaging/)

## Proposals

Pages here describe intended behaviour, not the code as it is. Each is rewritten to describe what was built when it lands, and the pages it changes are amended in the same change.

- [Setup sessions and config change sets](setup-sessions/)

## Keeping these pages current

If a change alters behaviour a page describes, update the page in the same change: the opening and body must stay true of the code. Record when and why something changed in the page's History section rather than in the body. The pages live in `site/src/content/docs/architecture/`, and `make docs-check` checks their example configs and commands like the rest of the site.
