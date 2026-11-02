---
title: Multi-repo changes
description: Track one change across several repositories: a parent issue whose agent files one issue per repository, waits for all of them to close, then reports the set of PRs.
---

Some changes span repositories: renaming a field in an API and its clients, or bumping a shared library everywhere it is used. Open Conduit tracks such a change as one parent issue. The parent's agent files one issue in each repository, and each is worked by that repository's own agent and lands as its own PR. The parent waits until every one has closed, then reports the set of PRs on the parent issue.

It needs no special support. Each repository is a [GitHub engineering](../github/) scope, and the parent's agent uses three operations every issue agent has:

- `github.issue.create` with `project` files an issue in another scope's repository;
- `github.wait` with several `<project>/` conditions parks the parent until any one of them is met;
- `github.issue.view` with `project` reads how each one ended.

## Example config

Three scopes: `platform`, where parent issues live, and `web` and `api`, where the changes land. Only `platform` needs grants: `file_scopes` to file and comment in the others, and `read_scopes` to wait on their issues. Each scope claims its own issues as usual.

```json
{
  "version": 1,
  "settings": {},
  "connections": [
    {"id": "github", "type": "github", "config": {}, "secret_refs": {"token": "cmd:gh auth token"}}
  ],
  "scopes": [
    {
      "id": "platform",
      "name": "acme/platform",
      "secret_refs": {},
      "authorization": {
        "connections": ["github"],
        "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view", "github.issue.list",
                       "github.issue.create", "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
        "read_scopes": ["web", "api"],
        "file_scopes": ["web", "api"]
      },
      "extensions": [
        {"name": "platform", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/platform", "scope": "platform", "repo": "acme/platform"}}
      ],
      "routes": [
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "agent",
          "executor": "claude-code",
          "connection": "github",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
          "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view", "github.issue.list",
                         "github.issue.create", "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
          "secret_names": [],
          "config": {"branch": "agent/issue-{number}"},
          "resource": {"provider": "git", "config": {"checkout": "/path/to/platform", "mode": "work"}}
        }
      ]
    },
    {
      "id": "web",
      "name": "acme/web",
      "secret_refs": {},
      "authorization": {
        "connections": ["github"],
        "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                       "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "web", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/web", "scope": "web", "repo": "acme/web"}}
      ],
      "routes": [
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "agent",
          "executor": "claude-code",
          "connection": "github",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
          "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                         "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
          "secret_names": [],
          "config": {"branch": "agent/issue-{number}"},
          "resource": {"provider": "git", "config": {"checkout": "/path/to/web", "mode": "work"}}
        }
      ]
    },
    {
      "id": "api",
      "name": "acme/api",
      "secret_refs": {},
      "authorization": {
        "connections": ["github"],
        "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                       "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "api", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/api", "scope": "api", "repo": "acme/api"}}
      ],
      "routes": [
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "agent",
          "executor": "claude-code",
          "connection": "github",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
          "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                         "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
          "secret_names": [],
          "config": {"branch": "agent/issue-{number}"},
          "resource": {"provider": "git", "config": {"checkout": "/path/to/api", "mode": "work"}}
        }
      ]
    }
  ]
}
```

## The parent's instructions

Put these in `acme/platform`'s `.open-conduit/playbooks/work-issue.md`, so every parent issue's agent gets them as additions to its playbook:

```md
# Changes across repositories

An issue here that asks for a change in other repositories is a parent: it lands no code of its own.

1. Work out what each repository needs. File one issue per repository with `github.issue.create` and its `project`
   (`web`, `api`): what to change there, why, and what the other repositories are doing. Link back to this issue.
   Check `github.issue.list` with the `project` first, so a retried turn doesn't file twice.
2. Comment on this issue with the list of child issues.
3. Wait for all of them with `github.wait`: one `issue:<project>/<number>:closed` condition per child, a `reason`,
   and an `until` a week or two out. Any one closing wakes you.
4. When you are woken, read each child with `github.issue.view` (`project`, `number`). Note which have closed and
   the PR that closed each. If some are still open, wait again on those alone.
5. When every child has closed, comment on this issue with each repository's PR and whether it merged, and end
   with `review`. If a child closed without its PR merging, say so and ask whether to file it again.
6. If one change must land before another (the API before its clients), file the later issues only once the
   earlier ones have closed.
```

## How it goes

1. You open "Rename `user_id` to `account_id` in the API and the web app" in `acme/platform`.
2. Its agent files "Rename `user_id` to `account_id` in the orders API" in `acme/api` and "Use `account_id` from the orders API" in `acme/web`, comments both links on the parent and waits on both.
3. Each repository's agent works its own issue and opens a PR there, which you review in that repository.
4. The API PR merges first. The parent wakes, sees one child still open, and waits on that one alone.
5. The web PR merges. The parent wakes, comments both PRs on the parent issue and ends in `review`. You close it.

## Limits

- **The children are issues, not child Work.** Each repository's change is an issue in that repository, so it is reviewed, discussed and merged there, and its agent works in that repository's checkout. Child Work made with `create_work` stays in the parent's scope, so it couldn't land a PR in another repository.
- **Nothing merges the PRs together.** Each PR merges on its own. If they must land together, say so in the child issues and merge them yourself in order.
- **A held child can't be filed in another project.** `github.issue.create` refuses `wait` and `until` with a `project`, so the parent files later stages once earlier ones close, as in step 6.
- **Every child repository needs a grant.** A project missing from `file_scopes` or `read_scopes` is refused, and the refusal names the grant to add.
