---
name: Agent task
about: One task, one commit, landed green — a body an agent can work cold.
title: ""
labels: agent-task
---

<!--
Sections are ordered by cost of getting them wrong, not by reading pleasure.
An agent re-reads the TOP of a ticket mid-work and rarely re-reads the bottom,
so anything whose violation is unrecoverable sits above the task detail.

A section you cannot fill means the ticket is not ready to work. Say that
rather than leaving it blank.

PUBLIC TRACKER. No media, no media hashes, no credentials, no vendor
endpoints, no harmful example URLs — in an issue body, a comment, or a
fixture. See AGENTS.md invariants 3 and 4.
-->

## Task

<!-- ONE imperative sentence: "Add X to Y so that Z." No history, no
     rationale — those live in the collapsed block at the bottom. -->

## Autonomy class

<!-- Pick one mechanically, not by vibe:
     A — the Do-not-touch list fully contains the change, no invariant named.
     B — an invariant is named below, or the change touches shipped binary code.
     C — needs a credential, a live vendor call, or a human judgement call. -->

- [ ] **A** — an agent may land this unattended
- [ ] **B** — an agent may land it; hold the PR for human review
- [ ] **C** — human-only; an agent may open a draft and stop

## Do not touch

<!-- Scope stated negatively, because a negative boundary is the only form
     checkable in one command. This closes the cheapest route to a green
     gate: editing the failing assertion instead of the code. -->

- **Frozen — these tests must pass UNMODIFIED:** <!-- e.g. internal/pipeline/rollup_test.go -->
- **Off-limits paths:** <!-- e.g. internal/config/thresholds.go -->
- **Forbidden additions:** `t.Skip`, `//nolint`, `//go:build` on a test file,
  `testing.Short()`, a lowered threshold default, a deleted test, an edited
  CI workflow — plus: <!-- anything specific to this ticket -->
- **Invariants in reach** (AGENTS.md § Invariants, numbered 1–8):
  <!-- #N — one line on how this change could weaken it. "none" is a valid answer. -->

Check before opening the PR:

```sh
git diff --name-only origin/main...HEAD                 # nothing off-limits
git diff -U0 origin/main...HEAD | grep -E '^\+.*(t\.Skip|//nolint|testing\.Short)'   # prints nothing
git diff --quiet origin/main...HEAD -- <frozen paths>    # exit 0
```

## The tempting wrong shape

<!-- The plausible-but-wrong implementation this ticket will attract, and the
     assertion that proves you did not ship it. This repo's recurring misfold
     is fail-open: something unscorable quietly becoming `allow`, or a null
     score quietly becoming 0. Naming it converts a tribal warning into a
     testable negative. "none" is valid for genuinely low-risk tickets. -->

- Wrong shape:
- The test that catches it:

## Premise

<!-- 1–3 falsifiable claims this ticket rests on, each pinned to a location and
     a revision. First thing to do is check them. A stale or false pin is a
     stop condition, not something to code around. -->

- `path/file.go:NN` @ `<sha>` — <claim>

## Scope

**In.**

**Out.**

## Acceptance criteria

<!-- Each criterion names the Go test identifier that proves it. A criterion
     with no named test is a judgement call at check time and does not belong
     here. A test name that turns out to be the wrong shape or the wrong layer
     is `blocked:underspecified` — say so, do not write the useless test as
     well as the useful one. -->

### Positive

- [ ] <behavior> — `TestXxx`

### Negative / edge

- [ ] <behavior> — `TestXxx`

### Done gate

- [ ] `gofmt -l .` prints NOTHING, then `go build ./... && go vet ./... && go test ./...` all pass
- [ ] Patch coverage ≥90%; uncovered lines and the reason named in the commit message
- [ ] Every doc under AGENTS.md § "Docs that must stay true" whose described behavior changed is updated in the SAME commit
- [ ] No media byte, media hash, secret, or provider `Raw` added to any envelope, log, audit record, queue payload, fixture, or UI surface
- [ ] Anything asserted but not runnable on this box is appended to `docs/agent/UNVERIFIED.md` with what would prove it
- [ ] <gates specific to this ticket>

**Red first.** Before writing implementation, run this ticket's AC commands
against unmodified `HEAD` and paste the raw output as the first comment. That
triages the ticket in one step: red as described means the work is real;
already green means the AC is stale; a build error means it is aimed at code
that does not exist yet. In the last two cases, stop — see below.

## Exit conditions — stopping is a completed outcome

A forced stub commit is a silent `allow`. An honest stop is `verdict: "error"`
plus human review, and this project already chose which of those it prefers.

To stop: push the branch, do **not** force a commit, apply the one matching
label, and post the block below as a comment.

| Condition | Label |
|---|---|
| A Premise claim is false at `HEAD` | `blocked:premise-wrong` |
| An AC cannot be checked without a judgement call | `blocked:underspecified` |
| Satisfying an AC requires weakening a listed invariant | `blocked:invariant-conflict` |
| Needs a `VISMOD_*` secret or a live vendor call this box does not have | `blocked:needs-credential` |
| 3 consecutive full done-gate runs fail for reasons this ticket did not scope | `blocked:gate-red` |

```stop
condition: <exactly one of the labels above>
evidence: <a file:line, a command, or a failing test name — "unclear to me" is not evidence>
gate: <the done-gate result, or "not run">
cheapest-unblock: <what would make this workable>
branch: <name> (pushed, N commits)
```

A stop is only admissible after reading the cited files and running
`go build ./... && go test ./...` once on an unmodified tree. A maintainer who
disagrees applies `stop-overruled` and reopens — a permanent public record, so
refusing is cheap only when it is right.

## Depends on / unblocks

- Depends on: #
- Unblocks: #

<details>
<summary>Why this matters — background, optional reading, never load-bearing</summary>

<!-- The reasoning, the design history, the alternative that was rejected.
     Anything an agent MUST act on belongs above this line, not inside here. -->

</details>
