# Simpleton — Full-Platform Roadmap

*Authoritative product plan. Revised 2026-08-26 from `FINDINGS.md`, `ASSESMENT.md`, and `NOTES.md`. `ASSESMENT.md` and `NOTES.md` are incorporated historical reviews; this document wins on product scope.*

## 1. Product claim

Simpleton gives engineering teams **semantic-regression evidence and replayable witnesses** for human- and agent-authored changes. It never reports that a change is safe, proved equivalent, or behavior-preserving.

Simpleton rents transformation and owns three components:

- **WITNESS** plans and executes evidence collection for arbitrary diffs.
- **SENSE** scopes external agents and presents a ranked simplification backlog to humans.
- **LEARN** records evaluation and production outcomes so methods can be calibrated over time.

External agents propose and implement transformations. Humans choose SENSE work, review Change Intents, and retain merge authority.

## 2. Evidence semantics

Every diff may receive deterministic baseline evidence. Semantic conclusions require a checked-in Change Intent whose Equivalence Contract is approved by the required code owner for the current pull-request head.

A repeatable pre/post difference is an **Observed Divergence**. It becomes a **Behavioral Witness** only when:

1. the input satisfies machine-checkable domain preconditions from the approved contract;
2. the observation violates its approved comparator; and
3. the result replays from an immutable Replay Capsule.

Missing intent, stale approval, unsupported methods, inconclusive analysis, flakiness, and exhausted budgets remain visible and nonblocking. The canonical language is defined in `CONTEXT.md`.

## 3. WITNESS

WITNESS is a local-first CLI and CI check. Its primary command is:

```text
simpleton analyze --repo <path> --base <sha> --head <sha> --output <dir> [--intent <path>] [--policy <path>]
```

When no intent is supplied, the same command emits a proposed manifest and keeps semantic results advisory. Approved manifests are retained under `.simpleton/intents/<stable-id>.yaml`; repository policy lives at `.simpleton/policy.yaml`.

WITNESS uses an applicability-driven evidence graph rather than a fixed ladder:

1. build, typecheck, existing tests, structural classification, and static/security deltas;
2. Verification Target extraction and Observation Boundary selection;
3. stable-caller or public-API differential execution;
4. native property/fuzz probes and generated dependent callers or harnesses;
5. mutation, adversarial generation, slicing, and longer fuzz campaigns;
6. replay, domain validation, contract validation, and minimization.

Unchanged callers and public APIs are preferred to generated dependent callers; direct changed-unit probes are a fallback. All applicable methods run synchronously under policy budgets: 30 minutes total by default and a 60-minute hard ceiling. Budget exhaustion is evidence, not a failure verdict.

The foundation-model semantic reviewer is present from launch through the managed plane. It is advisory, has no execution tools, and cannot create a Behavioral Witness or affect CI blocking.

## 4. SENSE

SENSE composes duplication, dead-code, complexity, dependency, policy, stale-flag, and coverage signals into a Scoping Pack. It serves two surfaces:

- machine-readable category, region, evidence, risk, coverage, and allowed file set for an external agent;
- a local HTML/Markdown backlog for human triage.

Backlog rank is maintenance benefit multiplied by WITNESS applicability, discounted by risk and review effort. A human selects an item before an external agent drafts the Change Intent and patch; SENSE never dispatches transformations autonomously.

## 5. LEARN

LEARN records Run Applicability, method yield, cost, latency, reviewer action, acceptance, review cycles, reverts, and escaped regressions. Local metadata uses SQLite and immutable artifacts use a content-addressed store. Evidence Packs remain standalone, exportable JSON.

LEARN does not build generic maintainability trend dashboards. Managed telemetry receives locally redacted Evidence Packs only after organization-admin opt-in and repository allowlisting. Raw managed packs expire after 30 days, are deletable, and are not used for training or cross-customer learning without separate consent.

## 6. Language and execution strategy

One Go core owns the CLI, evidence planner, policy, storage, reports, and managed-plane clients. Native language packs run out of process over newline-framed JSON-RPC 2.0:

- Go: native parsing, types, SSA/call graph, and effect analysis.
- TypeScript: compiler and type-checker APIs.
- Python: AST/CST, import, and type analysis.
- Swift: SwiftSyntax/SourceKit-family analysis when its phase begins.

The protocol and JSON schemas use explicit current versions; core, packs, and consumers upgrade together. A capability handshake rejects version mismatch rather than silently degrading.

Trusted branches execute in pinned, secret-free, network-disabled rootless containers. Public forks are deferred. Linux Swift follows the core packs; Apple-platform Swift later uses secret-free, network-disabled ephemeral macOS VMs with pinned Xcode/toolchains and a distinct trust class.

## 7. Pilots and qualitative decision

The public pilots calibrate the design; their small samples do not support statistical product claims.

### Pilot A — static reach

Inspect 10 historical PRs from each of two OSS repositories per core language: 60 PRs total. Compare direct changed-unit reach with stable-caller and public-boundary reach, and record extraction failures and infrastructure effort.

### Pilot B — replay

Evaluate 10 cases per language: five independently established, test-missed breaking changes and five preserving controls. Generated mutants may fill gaps only after their non-equivalence is independently witnessed.

The Simpleton lead scores each language from 1–5 on boundary constructibility, legal-input confidence, contract relevance, replay stability, infrastructure effort, and reviewer usefulness:

- **Continue:** average at least 4.0 and no critical dimension below 3.
- **Specialize:** average 3.0–3.99 or a critical weakness.
- **Defer:** average below 3.0.

All three core languages complete both pilots. Continue and specialize packs enter parallel implementation; defer postpones that pack. Earlier 20/10 reach bands and 20% effective recall remain diagnostics only.

Generated harnesses may become the default for a language/change class after at least 30 eligible targets and a one-sided 95% non-inferiority analysis excludes a native validated-witness-yield loss worse than 20 percentage points. Native methods are references, not ground truth.

## 8. Rollout

1. Reconcile evidence, plan, implementation, glossary, and ADRs.
2. Complete Pilot A and Pilot B for Go, TypeScript, and Python.
3. Build the Go core and qualifying native packs in parallel.
4. Run historical replay on a design-partner repository and repeat the qualitative rubric.
5. Enable live shadow CI and measure whether reviewers open, replay, dismiss, or act on evidence.
6. Let each partner opt into selective blocking through reviewed repository policy.
7. Add SENSE, its human backlog, and the external-agent handoff.
8. Expand Swift on Linux, then implement Apple-platform execution.

Block-eligible results are limited to build/typecheck regressions, existing-test regressions, policy-approved high-confidence static/security regressions, and mechanically validated Behavioral Witnesses. FM opinions, unvalidated divergences, generated-test failures, unsupported/inconclusive results, flakiness, and budget exhaustion never block.

## 9. Success measures

- PR-level Run Applicability within each declared change class.
- Stable-boundary reach versus direct-unit reach.
- Incremental detection beyond existing tests.
- Domain-valid and contract-relevant divergence rates.
- Invalid blocker count and partner-selected blocking precision.
- P50/P95 cost and latency against repository budgets.
- Reviewer action, time, and review-cycle count.
- Acceptance, revert, and escaped-regression outcomes.

These are reported with denominators and uncertainty. No metric is collapsed into a safety score.

## 10. Explicit exclusions

- No transformation engine or autonomous merge promotion.
- No Java production pack or Java baseline replication phase.
- No public-fork execution in the initial trust model.
- No generic maintainability trend dashboard.
- No claim that replay alone establishes a regression.
