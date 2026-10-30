# B05 configuration fixtures

These are complete version-1 documents for synthetic in-process registrations, not runnable service credentials or a release setup guide. `workspace-free.json` deliberately has no repository/resource; `git-scope.json` opts into the `git` resource; `shell-job.json` binds a shell executor and schedule extension; `inheritance.json` overrides a named route/binding and removes a secret; `sandboxed.json` runs the `git-scope` binding in an `srt` sandbox (B09) on a `clone` workspace. Its `checkout`/`mode` resource config is also valid for the delivered [Git provider](../../workspaces.md) (03-02); other real extension schemas arrive with their implementations.

`invalid-capability.json` is structurally valid but must fail semantic validation with this safe diagnostic (the schema alone cannot know executor capabilities):

```text
scopes/0/bindings/0/requires/inject: unsupported_capability: executor shell has no inject; select an inject executor or remove the requirement and set route unsupported_fallback=queue
```

The fixture verifier reports the stable code with an effective binding name in its internal diagnostic path; a production loader reports the original JSON pointer above. A fallback only permits an unsupported route policy; it cannot override an explicit `requires.inject=true` requirement.

Run from repository root:

```sh
python3 -m venv /tmp/open-conduit-config-contract
/tmp/open-conduit-config-contract/bin/pip install -r scripts/requirements-config-contract.txt
/tmp/open-conduit-config-contract/bin/python scripts/check-config-contract.py
```

The checker validates the Draft 2020-12 schema itself, all five positive documents and the negative capability document, then exercises negative mutations for unknown fields, zero causal limits, unsupported policies, literal/line-broken secret references, scope cycles/missing parents, duplicate names, widened authority/limits, missing bindings, denied secrets/connections, missing resources and invalid tombstones, and for a sandboxed binding a worktree mode, setup, an unknown provider, an executor without the capability and an unknown sandbox field. It also checks valid command references, duplicate JSON keys, whole binding replacement, stable route slots and restoration after disabling a slot. It makes no secret/provider/network/process calls and uses no runtime packages. It does not prove production config reload, auth, secret handling, registered Go optional interfaces or Unix socket behavior; those remain implementation conformance in 02-06/02-07.
