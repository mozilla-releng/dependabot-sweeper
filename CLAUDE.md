# CLAUDE.md — dependabot-sweeper

## ⚠ READ THIS BEFORE DOING ANYTHING ELSE

**You must read [`docs/PRINCIPLES.md`](docs/PRINCIPLES.md) in full before performing any design,
implementation, review, or planning work on this project. This is non-negotiable.**

`docs/PRINCIPLES.md` is the authoritative statement of *why* this tool exists and the constraints
that govern every decision made in it. Where the current code and `docs/PRINCIPLES.md` disagree,
the principles win and the gap is a bug to fix — not a reason to compromise the principle.

Do not proceed past this file until you have read and understood `docs/PRINCIPLES.md`. In
particular, read the **Agent empowerment principle** section carefully — it directly governs how
you should design any agent prompt, pipeline stage, or information-gathering step. Failing to
apply it is the most common source of design regressions in this project.

---

## Project orientation

Once you have read `docs/PRINCIPLES.md`, the other docs give you the full picture:

| Document | What it contains |
|---|---|
| [`docs/PRINCIPLES.md`](docs/PRINCIPLES.md) | **Start here.** The why — top-level design constraints that override everything else. |
| [`docs/ALGORITHM.md`](docs/ALGORITHM.md) | The current decision algorithm, from triage through to the implementation pipeline. |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | System components, data flow, and the Go package structure. |
| [`docs/WORKPLAN.md`](docs/WORKPLAN.md) | In-flight work, open questions, and the current phase of development. |
| [`docs/DASHBOARD.md`](docs/DASHBOARD.md) | The operator dashboard — what it shows and how it is structured. |
| [`docs/questions.md`](docs/questions.md) | Design Q&A — specific questions that arose and how they were resolved. |

Start with PRINCIPLES, then ALGORITHM (to understand the current design), then WORKPLAN (to see
what is actively in progress or deferred).
