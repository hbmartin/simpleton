# Simpleton — Technical Design

*Authoritative implementation design. Revised 2026-08-26 to implement `PLAN.md`. Where this document and `PLAN.md` differ on product behavior, `PLAN.md` wins.*

## 1. Repository shape

```text
cmd/simpleton/          Go CLI
internal/               planner, domain, execution, storage, reports, managed clients
packs/                   native TypeScript, Python, and Swift pack sources
schemas/v1/              current public JSON schemas and protocol shapes
docs/adr/                hard-to-reverse decisions
CONTEXT.md               canonical domain language
```

The Go pack is compiled into the Simpleton executable and spawned as a separate `pack go` process. TypeScript, Python, and Swift packs remain native executables. All packs are operationally isolated from the core.

## 2. Public command behavior

```text
simpleton analyze \
  --repo /workspace/repo \
  --base <git-revision> \
  --head <git-revision> \
  --output .simpleton/runs/<run-id> \
  [--intent .simpleton/intents/<id>.yaml] \
  [--policy .simpleton/policy.yaml]
```

The command resolves immutable baseline/candidate commits, loads policy and intent, verifies approval context, runs the graph, persists local state, renders reports, optionally uploads a redacted pack, and maps policy-selected evidence to its exit code:

- `0`: analysis completed without a selected blocker;
- `1`: repository policy selected at least one block-eligible result;
- `2`: invalid invocation, configuration, schema, protocol, or internal execution failure.

Exit code 2 is an infrastructure signal, not a semantic conclusion. Unsupported methods and ordinary per-method execution failures are recorded in the Evidence Pack and do not use exit code 2 once the run itself can complete.

If the intent is absent, the output includes `proposed-change-intent.yaml`; inferred semantic probes are advisory. If the intent exists but current-head code-owner approval is absent or stale, the contract is parsed and used for advisory analysis only.

## 3. Contracts

### Change Intent

```yaml
schema_version: "1"
id: stable-change-id
rationale: Consolidate duplicate price calculation
classification: behavior_preserving
scope:
  - package: pricing
allowed_changes:
  - allocation_count
equivalence_contract:
  api_mappings: []
  observations: []
```

`schema_version`, `id`, `rationale`, `classification`, `scope`, `allowed_changes`, and `equivalence_contract` are required. The manifest is retained under `.simpleton/intents/`. A changed contract requires fresh code-owner approval for the current PR head.

### Observation Spec

An Observation Spec identifies a Verification Target, setup, inputs or generator, preconditions, calls/events, observables, normalizers, comparator, and tolerances. Executable hooks are either reviewed built-ins or repository symbol references. Inline scripts are rejected.

Built-in comparators initially cover exact values, structural JSON, order-insensitive collections, exception type with optional message matching, and absolute/relative floating tolerance. Symbol hooks run in the same isolated environment as the target.

### Policy

`.simpleton/policy.yaml` defaults to shadow mode. It declares total/hard budgets, per-method budgets, trusted container images and commands, block-eligible categories, managed upload consent, organization/repository identity, and language-pack commands.

The core enforces block eligibility independently of policy: policy cannot promote advisory FM output, unvalidated divergence, unsupported/inconclusive work, flakiness, or budget exhaustion.

## 4. Evidence Pack

The Evidence Pack is versioned JSON containing:

- run identity, base/head tree digests, intent and policy digests;
- environment and trust-class provenance;
- Pack Capability and Run Applicability per language/target;
- Verification Targets and selected Observation Boundaries;
- per-method status, budget, duration, coverage, and findings;
- Observed Divergences and validated Behavioral Witnesses;
- Replay Capsule content digests and replay commands;
- SENSE Scoping Pack summary;
- advisory FM review;
- telemetry/redaction outcome and LEARN outcome links.

Per-method statuses are `ran`, `unsupported`, `inconclusive`, `budget_exhausted`, `flaky`, and `execution_failed`. Semantic observations are `divergence_confirmed`, `no_divergence_observed`, or `not_observed`. None maps to “safe.”

## 5. Evidence planner

The planner builds a DAG from repository policy, pack handshakes, Run Applicability, available budgets, and contract state. Cheap deterministic checks are scheduled before expensive probes, but their position does not imply globally stronger or weaker evidence.

```text
resolve revisions and approval
          │
          ▼
build · typecheck · tests · structural/static delta
          │
          ▼
targets → stable callers/public boundaries
          │
    ┌─────┼─────────┐
    ▼     ▼         ▼
 stable  native   generated dependent
 caller  probes   callers/harnesses
    └─────┼─────────┘
          ▼
strengthening · mutation · adversarial · long fuzzing
          │
          ▼
replay → precondition → comparator → minimization
```

All applicable nodes run in the online CI job within a 30-minute default and 60-minute hard ceiling. Context cancellation propagates over JSON-RPC and terminates pack processes. An exhausted node produces `budget_exhausted`; the graph continues where dependencies permit.

## 6. Language-pack protocol

Packs speak newline-framed JSON-RPC 2.0 over stdin/stdout. Required methods are:

- `initialize`: negotiate the exact protocol version and return Pack Capability;
- `analyze`: extract targets, caller/public-boundary candidates, effect risks, and opportunities;
- `probe`: execute a selected native or generated Probe;
- `cancel`: cooperatively stop a request before process termination.

The core rejects any version other than the current version. Requests include repository path, immutable revisions, changed files, contract digest, environment digest, seed, and budget. Responses never contain an aggregate verdict.

Native analysis strategy:

- **Go:** `go/parser`, `go/types`, package loading, SSA/call-graph facts where available, global-write and effectful-call screening.
- **TypeScript:** TypeScript compiler program/type checker, symbol references, import graph, async/global/effectful call screening.
- **Python:** `ast`, `symtable`, import graph, optional type facts, decorator/async/global/nonlocal and effectful-call screening. Dynamic uncertainty becomes Run Applicability evidence.
- **Swift:** SourceKit/SwiftSyntax-family parsing and index facts; Linux first. Apple targets require the macOS VM trust class.

## 7. Execution and replay

Repository commands run only for trusted branches. The rootless-container substrate uses pinned images, disables networking, removes secrets, mounts source read-only, supplies writable ephemeral work/cache directories, and records the runtime/image digest. Missing container support returns `unsupported` for executable methods.

Replay Capsules are immutable content-addressed bundles containing fixtures, generated source, serialized state, environment manifest, baseline/candidate artifacts, seeds, comparator identity, and commands. A witness is emitted only after replay and mechanical contract validation. Two matching repetitions are a flakiness screen, not proof of stability; history is retained by target.

Cache identity includes the full baseline/candidate trees, dependency lockfiles, generated code, pack/core versions, contract, policy, toolchain and container image, environment/locale, comparator, seed, and budget.

## 8. Local and managed data

SQLite stores run metadata, method results, SENSE opportunities, reviewer actions, acceptance, reverts, and escaped regressions. Large artifacts live under a SHA-256 content store. Standalone Evidence Packs reference but do not depend on the database.

The host process—not an execution container—may call the Simpleton-managed advisory model and telemetry service. Model output is untrusted text with no tools and is always advisory.

Managed upload requires all of:

1. organization-admin opt-in;
2. an allowlisted repository;
3. successful local secret/sensitive-data redaction; and
4. authenticated encrypted transport.

The managed copy is the redacted Evidence Pack. Retention is 30 days for raw packs; deletion is supported; training and cross-customer use are prohibited without separate consent. Upload/model failure does not change local evidence or blocking.

## 9. SENSE and LEARN

Each native pack returns detector opportunities with benefit, verifiability, risk, review effort, category, region, evidence, and allowed files. The core ranks opportunities by:

```text
rank = normalized_benefit * applicability_confidence
       / (1 + normalized_risk + normalized_review_effort)
```

SENSE emits `scoping-pack.json`, `backlog.md`, and `backlog.html`. It has no mutation or dispatch authority.

LEARN provides local outcome ingestion and persists run-to-review links. Generic code-health trends remain out of scope; the stored outcome model is limited to evaluation and product efficacy.

## 10. Generated-harness promotion

Generated callers and harnesses are experimental candidates until evaluated per language/change class on at least 30 eligible targets. Promotion requires a one-sided 95% non-inferiority analysis whose interval excludes a validated-witness-yield loss greater than 20 percentage points relative to the native reference. Every reported witness must still have valid preconditions and stable replay; compile failures and invalid inputs count against yield.

## 11. Delivery phases

1. Contracts, schemas, Go core, storage, reports, and protocol fixtures.
2. Pilot tooling and native Go/TypeScript/Python analysis packs.
3. Rootless command execution, replay, contract validation, managed clients, and shadow CI.
4. SENSE backlog and LEARN outcome ingestion.
5. Partner policy blocking and Git-host approval adapters.
6. Linux Swift pack, followed by ephemeral-macOS-VM Apple support.

The current repository implements the executable foundation and conservative abstention paths. Research pilots and partner rollout require external repositories and human labels, so their manifests and results are data produced after the tooling exists rather than fabricated fixtures.
