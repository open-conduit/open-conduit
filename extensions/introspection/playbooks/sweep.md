---
name: sweep
description: Fold the corrections agents have logged in this scope into the project's rule files, land them on the main branch (a push, or a PR), and mark the corrections handled. Open Conduit runs it once a day as the scope's standing sweep Work (or by hand as ordinary Work); `introspection.evidence` gives the corrections, the rule files, the guide if any, and how to land the commit. Started as /open-conduit:sweep.
---

# Sweep corrections

Agents log each correction they get with `github.correction` (a durable note with an `area` and a body). Fold them into the project's rule files and land them on the main branch.

## Your tools

Call an operation with the `call` tool (`mcp__open-conduit__call`), giving the `operation` and its `input` object:

| Operation | Input | Does |
| --- | --- | --- |
| `introspection.evidence` | `for`: `sweep` | this scope's corrections no sweep has handled yet, grouped by area and leading line (each group with its count, the note ids, the Work they came from and sample bodies), plus the settings: `rules` (the rule files), `guide`, `land` (`push` or `pr`) and `remote` |
| `introspection.notes.resolve` | `notes` (ids), `summary` | record the corrections you handled, so the next sweep leaves them out |

The settings say where the rule files are (for example `CLAUDE.md` or `AGENTS.md`, and the rule skills they list). If they name a guide, read it first: it says which file each kind of rule goes in and how rules there must be written, and it outranks the defaults here. Without one, each rule goes in the skill or section whose work it covers, or else under the closest heading of the main rules file.

The `area` of a correction is a hint, not the answer: a `writing` correction about which fields an issue asked for is about reading the request, not writing.

## Steps

1. `introspection.evidence` with `for` `sweep`. If `corrections.count` is 0, say "No corrections to sweep" and stop. Note the exact list of note ids: agents may log more while you work, and those wait for the next sweep.
2. Check the checkout you run in is on the main branch and no rule file has uncommitted changes. If either isn't true, say why in one line and stop; the corrections stay for tomorrow. Then `git pull --ff-only`.
3. Read every correction in the list and every rule file in full. Group the corrections by theme (the evidence groups are a starting point, not the answer). Several corrections about the same mistake become one rule. Drop anything a rule file already says, unless it keeps happening; then sharpen the existing rule rather than add a second one. Drop one-offs that only made sense for that issue.
4. Edit the rule files. Put each rule in its home, under the heading it belongs to, in the same voice: a short imperative, with a banned example when that helps. Don't record where a rule came from, and don't add a changelog.
   - Edit a skill's frontmatter `description` only when the rules it covers change, so it still says when to load it.
   - Don't create a new skill in a sweep. If a theme fits none of the homes, put it in the closest one and say so in your summary.
5. Commit only the rule files, with a message listing the rules added or changed and the file each went into. Land it as the settings' `land` says, on its `remote`:
   - **push**: commit on the main branch and push. If the push is rejected, `git pull --rebase` and push again.
   - **pr**: `git switch -c open-conduit/sweep-<YYYY-MM-DD>` first, commit there, `git push -u <remote> <branch>`, open the PR with whatever tracker access this sweep has (with the same list in the body), then `git switch <main branch>`, so the checkout is back where it was. Without tracker access, push the branch and give its name in your summary.
6. Once the push succeeds, record the corrections you read, and only those, with `introspection.notes.resolve`: the commit or PR in the summary. If nothing survived deduplication, resolve them anyway ("already covered") and don't commit.
7. Finish in one line: how many corrections were read, which rules were added or changed and where, and the PR's link if you opened one.

## Don't

- Don't commit anything in a sweep except the rule files.
- Don't write an issue, comment or PR body into the checkout: pass it on stdin (`-` for `<file>` or `--body-file`), or write it in a temporary directory outside the checkout. Nothing cleans up a stray file left there.
- Don't touch another project's files: a sweep works on the checkout it starts in.
- Don't report a defect about this sweep with `introspection.report-issue` unless it is a real new defect: Open Conduit's chain limits stop report loops.
