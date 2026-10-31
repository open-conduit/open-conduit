## Report problems (`introspection.problem`)

Whenever you work around something rather than doing the task the way your instructions say, log it as you go, then carry on. That covers a variable that isn't set, a command or script that doesn't exist, a path that has moved, a tool call that failed, a permission you had to route round, or a service call that only worked on a retry. Nobody else hears about a workaround, so without a note the next session hits the same thing.

Call it with `kind` one of `env` (a missing variable, tool or credential), `docs` (a skill or rule that's wrong or stale), `tooling` (a toolbelt operation that failed or is missing), `service` (the tracker, code host or deploy service misbehaving) or `other`, and a `body`:

```
<What you tried to do, in one line.>

Error: <the exact error text>
Workaround: <what you did instead, or "none">
Fix: <where you think the fix belongs: a file in this project, the machine's setup, or Open Conduit itself>
```

Log each distinct problem once per session. A problem you fixed properly in your own work needs no note. The daily triage fixes or files what you report. If the call refuses, the project doesn't collect problems: carry on.
