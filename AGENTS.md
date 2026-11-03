# Design

- Design agent-facing features from the interface in: an agent asks Open Conduit for an outcome through its toolbelt (`open-conduit` operations) and never needs to know about markers, labels or tracker conventions. When asked what agents should know about a feature, answer that, not how the mechanism works.
- Fixes and designs aim for the right fix, not the smallest one: work out what the model should be from the [core principles](site/src/content/docs/architecture/principles.md) and fix the cause, not the symptom. A tactical patch can be a step towards that, never the recommendation on its own.
- When asked for thoughts on an idea, bring the evidence for and against it and hold back speculative extras (new operations, events, UI, rollout) until asked. That means not padding a proposal, never settling for less than a correct one.

# Working on tickets

After completing each ticket, run its relevant checks, update its status and completion evidence in the local plan, and commit the ticket's changes before starting the next ticket. Use a Conventional Commit message that describes the completed work.

Keep `plan/` excluded through `.git/info/exclude`; do not force-add it to a commit. Commit only work belonging to the ticket and leave unrelated user changes untouched.
