# Architecture Decision Records (ADR)

This directory records **architecture-level** decisions for llm-gateway. Bug fixes / implementation details go through commit messages + PR descriptions; architectural choices, component boundaries, and backward-incompatible changes go through ADRs.

> An ADR is a **record of the decision process**, not a spec. Each ADR describes: the situation at the time, the options considered, why one was chosen, and the cost of the ones given up. This lets successors know "why it wasn't done that way," avoiding going back down the same road.

## Division of labor with `docs/architecture/`

| Directory | Nature | When to write |
|---|---|---|
| `docs/architecture/*.md` | The currently **effective** design spec (interface contracts, component responsibilities) | Implementation must conform to it; when the doc changes, update the code in sync |
| `docs/adr/####-*.md` | Record of a **historical decision** (including rejected options + trade-offs) | Written when proposing a change; archived once adopted |

`architecture/` is "what it looks like now"; `adr/` is "why it looks this way, and what alternatives were once considered."

## Status lifecycle

```
  Proposed ──→ Accepted ──→ Deprecated / Superseded by NNNN
      │
      └──→ Rejected
```

- **Proposed**: Submitted by the author, awaiting review; can still be changed or withdrawn.
- **Accepted**: Approved by the team, implementation begins; the ADR content is final and no longer changes (except metadata). If the decision needs to change later, write a **new** ADR marked as supersedes.
- **Rejected**: Discussed but not approved; kept as evidence of what was "once considered."
- **Deprecated**: The decision itself has been abandoned with no replacement (e.g., the component was removed).
- **Superseded by NNNN**: Replaced by a new ADR; the old ADR is kept for history.

**Important**: Do not edit the content directly after Accepted (except for the five status lines above); to change it, you can only write a new ADR that supersedes it. This is the essential difference between an ADR and a regular document — historical decisions must not be rewritten.

## File naming

```
####-kebab-case-title.md
```

`####` is a 4-digit sequence number (starting at 0001), globally unique and never reused. Even if an ADR is Rejected, its number is still kept.

## Template (copy this when writing a new ADR)

```markdown
# NNNN. <Short title>

* **Status**: Proposed
* **Date**: YYYY-MM-DD
* **Author**: <github handle>

## Context

Why is this decision needed? What is the current situation? What constraints / pain points are driving it?

Reference specific code locations: `internal/foo/bar.go:42`, commit hash, issue number.

## Options Considered

### Option A: <Short description>
- **How**: ...
- **Pros**: ...
- **Cons**: ...

### Option B: <Short description>
(same as above)

## Decision

We choose **Option X**.

Rationale:
- ...

## Consequences

### Positive
- ...

### Negative / Trade-offs
- ...

### Migration Path (if backward-incompatible)

Phase 1: ...
Phase 2: ...
Rollback plan: ...
```

## Current ADR index

| ADR | Status | Decision |
|---|---|---|
| [0001](0001-explainable-virtual-model-routing.md) | Accepted | Resolve virtual-model policy before dispatch |
