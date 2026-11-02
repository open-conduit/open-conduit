---
title: Cap spend and attempts
description: Set budgets so unattended Work stops at a limit you choose, warns you before it gets there, and waits for you to raise the limit or let it go on.
---

Open Conduit can cap what Work consumes: how many attempts or turns one Work gets, how much one Work, one scope or everything together may spend in a day or a week. The daemon checks the budgets before it starts each turn. When a budget is used up the Work is held with the reason instead of starting again, and it waits for you.

## Set budgets

Budgets go in `settings.budgets` in the config file:

```json
{
  "settings": {
    "budgets": [
      { "unit": "attempts", "per": "work", "max": 20 },
      { "unit": "usd", "per": "work", "max": 15 },
      { "unit": "usd", "per": "scope", "period": "day", "max": 40, "scope": "open-conduit" },
      { "unit": "usd", "per": "all", "period": "week", "max": 250, "warn_at": 0.9 }
    ]
  }
}
```

Each budget says:

| Field | Means |
| --- | --- |
| `unit` | what is counted (see below) |
| `per` | `work` (each Work has its own count), `scope` (each scope has one count for all its Work) or `all` (one count across every scope) |
| `period` | `day` or `week` (from Monday), in UTC. Without it, the count never resets |
| `max` | the limit. A turn doesn't start once the count has reached it |
| `warn_at` | the fraction of `max` at which a warning fires (default `0.8`) |
| `scope` | only Work in this scope counts towards this budget, and only that Work is held by it |

Apply the change as usual with `open-conduit config apply FILE --generation N` ([configuration](../configuration/)). A count starts when its budget is first configured; usage from before then isn't counted.

## Units

The daemon counts two units itself:

- `attempts`: each new attempt at a Work, a retry included.
- `turns`: each time an attempt starts or resumes. Work that waits on events between turns, such as an issue whose agent wakes on each check result and comment, has one turn per wake.

Any other unit is what the executor reports. What counts as a cost is up to each executor:

- Claude Code reports its cost in `usd` and the input plus output `tokens`.
- The shell executor reports the command's wall time in `minutes`.
- An installed executor can report any unit it likes, such as `usd`, `tokens`, `minutes` or `builds`, on its result's usage.

A budget on a unit no executor reports never fills.

## When a budget is used up

The next turn of each Work that the budget covers doesn't start. The Work is held with a reason such as:

> Over budget: 15.2 usd used of 15 usd per Work. Release the hold to let one more turn start, or raise the limit.

Held Work shows under **Held** in the web view's Needs you list and in the TUI, and `open-conduit doctor` lists it in its `budgets` check. Input keeps arriving while it is held, so the next turn sees everything that came in. The Work's history records the hold as a `core.budget.held` event.

You then have two choices:

- **Raise the limit** in the config and apply it, then release the hold. The Work and everything else the budget covers can run until the new limit.
- **Release the hold** without changing the config (`H` in the TUI, Release in the web view, or `open-conduit work release ID --revision N`). This lets one more turn start. If the budget is still used up, the turn after that is held again.

A budget hold doesn't lift when a new day or week begins. Release it when you're ready for the Work to go on.

## Warnings

When a count crosses a budget's `warn_at`, the daemon accepts a `core.budget.warning` event in the Work's scope. When it reaches `max` it accepts a `core.budget.exceeded` event. Each fires once per budget and period, and again if you change `max`. Their data (schema `core.budget.v1`) names the budget in words (`budget`), the `unit`, `max`, the amount `used`, the `window`, and the `work_id` and `scope_id` that crossed it.

The events aren't addressed to the Work, so they never wake it. A route in the scope can act on them, for example to post a message in Slack. Without one, they are kept in the event history and telemetry.
