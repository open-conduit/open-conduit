# Design

- Design agent-facing features from the interface in: an agent asks Open Conduit for an outcome through its toolbelt (`open-conduit` operations) and never needs to know about markers, labels or tracker conventions. When asked what agents should know about a feature, answer that, not how the mechanism works.
- When asked for thoughts on an idea, lead with the smallest version that would work and the evidence for or against it. Offer the fuller design (new operations, events, UI, rollout) only when asked.

# Working on tickets

After completing each ticket, run its relevant checks, update its status and completion evidence in the local plan, and commit the ticket's changes before starting the next ticket. Use a Conventional Commit message that describes the completed work.

Keep `plan/` excluded through `.git/info/exclude`; do not force-add it to a commit. Commit only work belonging to the ticket and leave unrelated user changes untouched.
