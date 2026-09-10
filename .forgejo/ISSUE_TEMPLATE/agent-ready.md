---
name: Agent-ready task
about: Structured task for autonomous consumption by coding agents (RAGE / Auto-RAGE)
title: '[agent] '
labels:
  - agent-ready
---

<!-- Fill every section. An agent consumes this issue without human context:
     it must be able to verify Evidence, implement Requirements, and prove
     Acceptance criteria from the repository alone. -->

* Evidence

- Verified facts only: `file:line` pointers, observed behavior, API
  constraints, or prior issue references. No speculation; anything unknown
  belongs in a separate "Unknowns" list.

* Requirements

- Numbered, testable invariants. Each one must be verifiable from the
  repository alone after implementation.

* Acceptance criteria

- Deterministic checks that prove completion: exact commands, tests to add,
  or observable outputs. Always include the repository gate:
  `go build ./... && go vet ./... && go test ./...`.

* Scope guard

- What the consuming agent must NOT change. The smallest coherent change
  that satisfies the acceptance criteria wins; anything else gets its own
  issue.

* Order and dependencies

- Declare priority and blockers ("Blocked by #N") so the queue can be
  consumed strictly in order. No blocker means immediately eligible.
