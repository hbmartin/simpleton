# Overall assessment

**Proceed with the core idea, but fund an experiment—not the full roadmap.**

The three documents form a coherent and unusually self-critical proposal:

* `FINDINGS.md` makes the case that verification, rather than transformation, is the durable opportunity.
* `PLAN.md` turns that into **SENSE → rented agent → PROVE → LEARN**.
* `IMPLEMENTATION.md` adds capability manifests, evidence profiles, thin language packs, portable harness generation, and explicit degraded support rather than pretending every language is equally verifiable.   

The strategic inversion—**rent code generation, own executable evidence**—is sound. But the current proposal still overstates three things:

1. How unoccupied the verification space is.
2. How strong some of the proposed “oracles” are.
3. How close a replayable pre/post difference is to proving a meaningful regression.

My recommendation is to build a **single-language, shadow-mode semantic-regression witness engine for explicitly behavior-preserving diffs**. Do not initially build a four-language platform, a general “proof” system, or a blocking gate for arbitrary changes.

A defensible initial promise is:

> Given a diff declared behavior-preserving and an explicit observation contract, Simpleton attempts to produce a replayable, domain-valid semantic-regression witness. Otherwise, it reports exactly what evidence ran, what was observed, and what remained unverified.

That is narrower than “prove this diff is behavior-preserving,” but much stronger commercially and technically than generic AI code review.

---

# What the proposal gets right

## 1. The build-versus-rent boundary

The decision not to compete with transformation engines is the strongest strategic choice in the documents. Moderne already exposes deterministic semantic search and OpenRewrite transformations to coding agents; conventional codemod and migration tooling is also well established. Transformation quality will continue to improve as agents improve, while evaluation corpora, replayable witnesses, execution infrastructure, and trusted evidence histories are more durable.

The moat is therefore not the seven-stage workflow itself. It is:

* A corpus of real preserving and breaking changes.
* Reliable extraction of the relevant observation boundary.
* Extremely low false-blocking rates.
* Reproducible execution across repositories and toolchains.
* Evidence that materially reduces reviewer effort.
* Historical calibration of which checks work for which change classes.

## 2. Explicit capability and abstention

The implementation’s requirement that checks return `ran`, `unsupported`, or `budget_exhausted`, rather than disappearing silently, is important. So is reporting eligibility over the changed surface rather than presenting broad language support as broad verification coverage.

This should become even more central: capability is not merely language-specific. It is also repository-, target-, build-system-, and change-specific. A Go package containing deterministic functions may have excellent differential coverage, while a different Go service dominated by network calls and mutable global state may have almost none.

## 3. Evidence rather than a score

The proposed audit pack is more valuable than an aggregate “safety score.” A reviewer can act on a reproducible example, failing test, changed static finding, or unsupported scope. A score usually hides precisely the uncertainty the system is supposed to expose.

## 4. PROVE before SENSE

Starting with verification of diffs Simpleton did not generate is correct. It keeps the project useful even if:

* Frontier agents stop benefiting from deterministic scoping.
* Customers use several competing agent harnesses.
* Refactoring generation becomes commoditized.
* SENSE never produces enough incremental value to justify its complexity.

---

# Web findings that materially change the plan

## 1. SemaDiff is close prior art

The largest missing item is **SemaDiff**, a July 2026 paper that is much closer to the proposed core than the competitive discussion acknowledges.

SemaDiff identifies modified code and unchanged dependent callers, generates an additional dependent class to exercise the changed behavior, and runs the same tests against the pre- and post-change versions. On 183 Java refactoring commits, it reported 75.95% overall accuracy, 100% precision for detected semantic-changing refactorings, and 58.89% recall. Generated dependent code was responsible for nearly all detected semantic changes, and was necessary in many cases where the changed code had no usable original caller. ([arXiv][1])

This does **not** invalidate Simpleton. SemaDiff is a Java research prototype evaluated on seven repositories, not a cross-language operational CI product. But it does invalidate the broad statement that no one has integrated changed-code analysis, generated callers, and pre/post execution into a semantic gate.

### Decision and implication

Add SemaDiff as the nearest baseline and adopt its most important architectural insight:

> The primary verification target should often be an **unchanged caller or public observation boundary**, not the changed function in isolation.

Simpleton’s differentiation should be:

* Multi-language operationalization.
* Explicit evidence and applicability profiles.
* Domain-valid witness checking.
* Production-grade execution and caching.
* Integration with arbitrary agent and human diffs.
* Review-time reduction and very low false-block rates.

It should **not** claim novelty merely from running the same inputs against two versions.

---

## 2. The foundation-model “oracle” should not be an oracle

The paper behind rung 5 is much narrower than the headline suggests.

Its main dataset contains 226 known Java refactoring bugs, but 185 are compilation failures and only 41 are behavioral changes. The examples are also very small. The principal benchmark contains positive bug instances without a normal set of true negatives, so the reported “accuracy” is effectively recall on known failures rather than deployment-grade precision. ([arXiv][2])

On behavioral changes specifically, GPT-5.4 at deterministic temperature detected only 58.5%. In the project-diff feasibility study, 18 of 44 cases were returned as `UNKNOWN`; the apparently correct cases were generally supported by author intent rather than independently proven. Meanwhile, ordinary compilation caught the compilation-error majority, and SafeRefactor caught 221 of the 226 known bug cases. The authors themselves frame foundation models as lightweight triage aids, not complete correctness oracles. ([arXiv][2])

### Decision and implication

Remove “foundation-model oracle” from the externally described verification ladder. Rename it something like:

* **Semantic risk reviewer**
* **Dynamic-check router**
* **Suspicion classifier**

It should:

* Be advisory.
* Support `UNKNOWN` or abstention.
* Never independently block a merge.
* Be measured on balanced preserving and breaking diffs.
* Be evaluated separately per language.
* Earn its place only by increasing the yield or reducing the cost of stronger checks.

For the MVP, I would defer it entirely unless it is necessary to select which targets receive expensive differential analysis. Compiler, tests, static deltas, and executable witnesses are more legible and easier to calibrate.

---

## 3. A replayable counterexample is not automatically valid

The implementation says that the harness does not need to be trusted because a counterexample validates itself by replay. That is only partly true.

Replay establishes **execution validity**:

> Under this environment, these two artifacts produced different recorded observations for this input.

It does not establish:

1. **Domain validity:** Was the input legal and reachable under the program’s actual preconditions?
2. **Contract relevance:** Is the observed difference part of behavior that was intended to remain stable?

A harness may find divergence in:

* Exception wording that is not contractual.
* Iteration order that callers may not rely on.
* Floating-point rounding below an accepted tolerance.
* Reflection or identity behavior deliberately changed by a refactor.
* An invalid object state that production constructors cannot create.
* Timestamps, randomness, generated identifiers, or nondeterministic scheduling.
* Undefined or implementation-dependent behavior.

The differential-fuzzing evidence is still valuable, but it comes from Python benchmark programs with relatively clean function-level reference behavior. It is not evidence that arbitrary production inputs or observations can be interpreted without a domain contract. More broadly, recent work on the “verification horizon” emphasizes that automated verifiers remain proxies for human intent and must be evaluated for faithfulness, robustness, and applicability—not just execution success. ([arXiv][3])

### Decision and implication

Replace `Counterexample` with a richer **`BehavioralWitness`**:

```text
BehavioralWitness
  target
  intent_contract
  observation_spec
  preconditions
  fixture_or_input
  before_observation
  after_observation
  comparator
  environment_digest
  baseline_artifact_digest
  candidate_artifact_digest
  replay_command
  replay_count
  domain_validity_evidence
  contract_relevance_status
```

A witness should only block when all three are established:

* It replays.
* Its input satisfies the declared domain.
* The changed observation violates a declared invariant.

Otherwise, it should be reported as an **interesting divergence requiring review**, not a confirmed regression.

---

## 4. Change intent is a missing first-class input

Verification cannot determine whether a difference is a bug without knowing whether behavior was supposed to remain unchanged.

The current architecture accepts arbitrary diffs, which is useful for evidence generation, but insufficient for blocking decisions. A feature diff, bug fix, migration, performance change, and pure refactor have different allowed observations.

### Decision and implication

Add a `ChangeIntent` or `EquivalenceContract` before PROVE:

```yaml
intent: behavior_preserving
scope:
  - package: pricing
allowed_changes:
  - log_messages
  - allocation_count
preserve:
  - public_return_values
  - public_error_types
  - persistent_state_transitions
  - emitted_events
api_mapping:
  before: OldPriceCalculator.calculate
  after: PriceCalculator.calculate
```

For agent-generated diffs, the agent can propose this contract before editing. For human PRs, use a small checked-in manifest or PR form. Without a contract, PROVE remains advisory.

This also narrows the initial market: Simpleton should first target diffs that explicitly claim to preserve behavior, rather than all code changes.

---

## 5. SWE-ABS should not be copied directly into an online rung

SWE-ABS provides strong evidence that existing tests accept incorrect patches. But its test-strengthening method is built around benchmark instances with an issue statement, a gold patch, and a test patch. Its authors report that augmented tests can reject valid alternative fixes, with a measured false-negative/overfitting problem, and the reported workflow averaged roughly 18.5 minutes and $2.50 per benchmark instance. ([arXiv][4])

For a declared refactor, the pre-image can sometimes act as a behavioral reference. That makes the idea more applicable than it is for arbitrary bug fixes, but it does not eliminate observation and domain problems.

### Decision and implication

Split rung 6 into two products:

**Online, per-PR**

* Changed-region coverage analysis.
* Cheap generated regression probes.
* Stable-caller or public-API differential execution.
* Small mutation budgets around the changed observation.
* Strict false-blocking controls.

**Offline or high-risk**

* Larger mutation campaigns.
* Adversarial test synthesis.
* Program slicing.
* Long fuzzing campaigns.
* Cross-model or multi-strategy generation.

Do not put the full adversarial strengthening system in blocking CI until it demonstrates adequate precision and latency on real preserving diffs.

---

## 6. The seeded-mutation evaluation is not actually free to label

The proposed evaluation treats mutation tools as a free source of known semantics-breaking changes. Mutation tools do provide inexpensive candidate faults, but not every mutant is behavior-changing. Both PIT and Stryker explicitly document the **equivalent-mutant problem**: some generated mutants are observably equivalent to the original, and Stryker has no definitive automatic way to identify and exclude them. ([PIT Mutation Testing][5])

There is also a representativeness issue. A verifier can perform well on simple operator substitutions while missing the kinds of state, dependency, lifecycle, serialization, or API errors that agents introduce.

### Decision and implication

The eval harness remains essential, but change its claim from “self-supervised ground truth” to **curated fault injection**.

Use four complementary datasets:

1. Real, confirmed behavior-breaking refactorings.
2. Real, confirmed behavior-preserving refactorings.
3. Generated mutants whose non-equivalence has been independently witnessed or adjudicated.
4. Recent private or design-partner agent diffs, reducing benchmark contamination risk.

Stratify by:

* Fault type.
* Change type.
* Language.
* Effectfulness.
* Existing-test detection.
* Observation boundary.
* Whether the verifier had access to an intent contract.

The primary product metric remains **faults caught that existing tests miss**, but the labels need independent validation.

---

## 7. Synthesized harnesses are promising, but not yet the portable foundation

Cleverest is meaningful evidence: its change-directed generated tests found comparable numbers of bugs in under two minutes to a directed greybox fuzzer given 24 hours, and generated tests improved subsequent fuzzing. But its subjects are parsers, interpreters, compilers, and similar programs with structured, human-readable inputs. HarnessLLM also demonstrates the value of specialized harness-generation training, but in a constrained competitive-programming-style setting. ([Conference Publishing][6])

Neither result establishes that an off-the-shelf model can reliably construct useful twin harnesses for arbitrary business logic across TypeScript, Python, Go, and Swift.

### Decision and implication

Treat generated harnesses as an **experimental candidate generator**, not the architectural foundation.

Go’s native fuzzing should be a reference baseline, not “ground truth.” Compare native and synthesized approaches using:

* Valid witness yield.
* Unique witnesses found.
* Changed-code and observation coverage.
* Invalid-input rate.
* Harness compile rate.
* Cost and wall time.
* Shrinking/minimization quality.
* Stability across repeated generation runs.

Only make synthesis the portable default after it approaches native approaches on common eligible targets. A failure here would not kill PROVE, but it would narrow multi-language rung-7 support.

---

## 8. The market is more crowded than “no incumbent,” but the exact wedge remains available

Sonar acquired Gitar in May 2026 and now explicitly markets AI code verification and governance. Qodo offers full-repository PR review and policy enforcement. CodeRabbit supports pre-merge custom checks and inconclusive outcomes, although its custom review checks do not themselves execute arbitrary repository code. Moderne already provides deterministic semantic operations to agent workflows. ([SonarSource][7])

So the category **“AI code verification”** is occupied. The more specific category remains comparatively open:

> Executable, replayable semantic-regression witnesses for changes that claim to preserve behavior.

### Decision and implication

Do not build a generic PR review interface. Ship:

* A CLI.
* A CI check.
* A machine-readable evidence API.
* SARIF/JUnit-compatible artifacts where appropriate.
* Thin adapters to GitHub, GitLab, agent harnesses, and existing review products.

The moat should be the evidence engine and its evaluation history, not another PR-commenting surface.

Potential partners and integration surfaces may ultimately be more valuable than competing directly with Sonar, Qodo, or CodeRabbit.

---

## 9. The economic evidence should be reframed

The 46.41% agent-PR rejection result is real, but rejection is not synonymous with technical incorrectness. A newer analysis of more than 11,000 closed agentic PRs attributed only 35.7% of rejected PRs to clear agent failures; workflow constraints and cases with no observable rationale accounted for much of the rest. Reviewer engagement is also strongly associated with integration outcomes. ([arXiv][8])

Similarly, METR’s early-2025 randomized study found experienced developers 19% slower with AI tools, but METR now labels that result historical. Its February 2026 follow-up says the newer experiment could not produce a reliable speed estimate because of selection and measurement effects, while suggesting that tools probably had become more useful. ([Metr][9])

### Decision and implication

Do not make “developers are slower with AI” or “half of agent PRs fail” the load-bearing commercial case.

The stronger premise is:

* AI increases the volume of plausible code.
* Existing tests and review processes do not fully establish semantic safety.
* Reviewers need higher-quality evidence and smaller uncertainty sets.
* Concrete witnesses are easier to act on than model opinions.

The commercial hypothesis still needs a controlled design-partner study showing reduced review effort or fewer escaped regressions.

---

# Decisions I recommend making now

| Decision                      | Recommendation                                                                                    | Implication                                                                                     |
| ----------------------------- | ------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| **Project status**            | Treat this as a design-partner product experiment first                                           | Avoid committing to a large platform before technical and commercial value are measured         |
| **External product claim**    | “Semantic-regression evidence and replayable witnesses”                                           | Avoid “proof,” “safe,” or universal behavior-preservation claims                                |
| **Initial change class**      | Small, declared behavior-preserving diffs with stable interfaces and deterministic observations   | Higher precision and measurable applicability, at the cost of narrower coverage                 |
| **Gate policy**               | Shadow mode first; block only deterministic failures or validated behavioral witnesses            | Slower rollout, but protects trust and avoids false-positive fatigue                            |
| **Reference language**        | Go for the technical lab                                                                          | Best environment for testing fuzzing, eligibility, isolation, and execution contracts           |
| **First production language** | Choose based on a real design partner, probably TypeScript or Python—not architectural preference | Validates market demand rather than merely tool availability                                    |
| **Java**                      | Keep as a benchmark and prior-art replication ecosystem                                           | Access to SemaDiff, refactoring corpora, SafeRefactor, and RefactoringMiner                     |
| **Swift**                     | Design for `unsupported` and degraded capabilities, but defer implementation                      | Avoid macOS execution, isolation, and build-cost complexity before product value is established |
| **Foundation-model reviewer** | Advisory or deferred; never an independent gate                                                   | Reduces false confidence and prevents a weak rung from becoming the easiest default             |
| **Test strengthening**        | Lightweight online path; heavy mutation/adversarial work offline                                  | Keeps CI usable while preserving a route to stronger evidence                                   |
| **Synthesized harnesses**     | Benchmark as an experiment against native approaches                                              | Multi-language expansion becomes contingent on measured performance                             |
| **SENSE**                     | Minimal detector composition only after PROVE works                                               | Prevents investment in a potentially commoditized scoping layer                                 |
| **LEARN trends**              | Keep the evaluation ledger; defer broad code-health dashboards                                    | Focuses learning on product efficacy rather than generic maintainability analytics              |
| **Distribution**              | CLI/API/CI integrations, not another review UI                                                    | Complements existing platforms and keeps the core portable                                      |

---

# Revised architecture

The current fixed seven-rung ladder should become an **evidence-planning graph**. The checks are not strictly monotonic: a compiler error can be definitive, an FM opinion is weak, a fuzz difference can be irrelevant, and an unchanged-caller test can be stronger than a generated function-level harness.

A cleaner structure is:

```text
ChangeIntent / EquivalenceContract
                  │
                  ▼
        Baseline deterministic checks
       build · tests · static/security delta
                  │
                  ▼
        VerificationTarget extraction
 symbol · module · unchanged caller · public API · scenario
                  │
                  ▼
         ObservationBoundary selection
        inputs · state · outputs · side effects
                  │
       ┌──────────┼───────────┐
       ▼          ▼           ▼
 stable-caller   native      generated dependent
 differential   property /   callers, tests,
 execution      fuzzing      or harnesses
       └──────────┼───────────┘
                  ▼
       Witness validation and minimization
 replay · domain validity · contract relevance
                  │
                  ▼
             Evidence pack

FM semantic reviewer: advisory branch used only for routing or explanation
Offline strengthening: mutation, adversarial tests, long fuzz campaigns
```

## Contracts to change

### Replace `ChangedUnit` with `VerificationTarget`

A function is not always the correct semantic atom. A change may affect:

* Class initialization.
* Dynamic dispatch.
* Serialization.
* Package-visible state.
* Persistent state transitions.
* A request-response scenario.
* Several functions whose behavior is only meaningful together.

Use:

```text
VerificationTarget
  scope_type: symbol | module | package | api | scenario
  baseline_artifacts
  candidate_artifacts
  dependencies
  observation_candidates
  eligibility
  exclusions
```

### Add `ObservationSpec`

This describes what actually counts as behavior:

```text
ObservationSpec
  setup
  inputs
  preconditions
  calls_or_events
  observable_outputs
  observable_side_effects
  normalization
  comparator
  tolerated_differences
```

### Separate pack capability from run applicability

Use two layers:

```text
PackCapability:
  what the Go/Python/TS/Swift adapter can theoretically do

RunApplicability:
  what can run for this repository, target, diff, and environment
```

This prevents “Go supports fuzzing” from being confused with “this changed Go service was fuzzable.”

### Replace binary verdicts

Use:

```text
divergence_confirmed
no_divergence_observed
inconclusive
unsupported
execution_failed
```

`no_divergence_observed` must always include budget, coverage, observation boundary, and eligibility. It must never be rendered as `safe`.

---

# Technical changes to make before implementation

## 1. Expand the cache identity

The proposed cache key based primarily on pre/post blobs and config is insufficient. Verification results also depend on:

* The complete relevant repository tree.
* Dependency lockfiles.
* Compiler and runtime versions.
* Generated code.
* Test and harness artifacts.
* Environment variables and locale.
* Container or runner image.
* Seed, budget, and comparator.
* Network and filesystem policy.

Use content hashes for the baseline and candidate worktrees plus an immutable execution-environment digest.

## 2. Do not treat native fuzzing as ground truth

Native fuzzing is a useful reference implementation, but it also has finite budgets, incomplete generators, and observation limitations. The harness comparison should ask which approach finds more independently validated witnesses, not which matches the presumed oracle.

## 3. Treat two repeated runs as a flakiness screen

Two runs can reveal nondeterminism, but two matching runs do not establish stability. Store historical flakiness by target, support more repetitions for noisy observations, and use domain-specific comparators where necessary.

## 4. Define execution trust classes

At minimum:

* Trusted private branch.
* Untrusted agent-generated diff in a trusted repository.
* External fork or public contribution.
* Apple-platform/macOS execution.

The MVP should run only trusted repositories and branches in network-disabled, secret-free, ephemeral Linux environments. Public-fork execution and macOS runners should come later.

## 5. Preserve artifacts, not merely logs

A useful witness may require fixtures, generated source, serialized state, a database snapshot, or a sequence of calls. Store a replay capsule containing everything needed to reproduce the observation—not merely a command that assumes the original CI workspace still exists.

---

# Recommended MVP scope

The first supported verification contract should be deliberately restrictive:

* The PR declares itself behavior-preserving.
* The public interface is unchanged or supplies an explicit before/after mapping.
* No network, filesystem, clock, randomness, concurrency, or uncontrolled global state.
* The changed surface contains no more than a small number of verification targets.
* Both revisions build under the same pinned environment.
* A stable caller, public API, or generated input boundary exists.
* Outputs and relevant side effects can be normalized and compared.

This will produce lower apparent coverage across all PRs, but much higher meaningful coverage within the declared class. That is preferable to a broad tool that quietly runs only weak checks.

Likely first use cases are:

* Agent-generated cleanup and simplification PRs.
* Extract/inline/move operations on deterministic library logic.
* Deduplication or conditional simplification with stable APIs.
* Mechanical migrations expected not to alter external behavior.
* Refactor campaigns where a reviewer already expects behavioral equivalence.

Avoid initially:

* Concurrency.
* Database migrations.
* Distributed workflows.
* UI rendering.
* Time-sensitive logic.
* Performance-only equivalence.
* Changes intentionally modifying edge-case behavior.
* Full architectural rewrites.

---

# Evaluation and go/no-go criteria

The evaluation should answer four distinct questions:

1. **Can it find defects existing tests miss?**
2. **Can it avoid blocking valid changes?**
3. **Can it cover a commercially meaningful fraction of the chosen change class?**
4. **Does the evidence save reviewers time or prevent regressions?**

## Core metrics

| Metric                                           | What it prevents                                         |
| ------------------------------------------------ | -------------------------------------------------------- |
| **Blocking precision**                           | False-positive fatigue and loss of trust                 |
| **Incremental recall beyond existing tests**     | Becoming an expensive wrapper around CI                  |
| **Domain-valid witness rate**                    | Reporting illegal or unreachable inputs                  |
| **Contract-relevant witness rate**               | Flagging harmless implementation differences             |
| **Eligibility within target class**              | Hiding near-zero coverage behind broad language claims   |
| **Inconclusive and infrastructure-failure rate** | Operational unreliability                                |
| **P50/P95 latency and cost**                     | An unusable CI gate                                      |
| **Review time and review-cycle count**           | A technically interesting product with no economic value |
| **Escaped-regression and revert rate**           | Failure to affect real outcomes                          |

## Provisional management gates

These are recommended product gates, not findings from the cited studies:

* **Blocking precision:** at least 99%, with no severe invalid blockers in the latest holdout.
* **Incremental detection:** catch at least 20% of independently validated, test-missed faults within the selected supported scope.
* **Eligibility:** at least 20% of PRs in the explicitly chosen behavior-preserving change class—not 20% of all PRs.
* **Operational failures:** below 5% for otherwise eligible targets.
* **Online latency:** P95 within roughly ten minutes; expensive campaigns run asynchronously outside the merge gate.
* **Reviewer impact:** at least a 20% reduction in review time or a material reduction in back-and-forth review cycles.
* **No binary safety label:** even after promotion to blocking mode.

If eligibility is below 10% even within the narrow target class, the generic equivalence-gate hypothesis is weak. At that point, specialize further—for example, parser-like code, pure transformation libraries, or particular migration classes—or pivot toward test-strengthening rather than broad verification.

---

# Recommended build sequence

## Step 0 — Correct the evidence and product language

Before coding:

* Add SemaDiff to `FINDINGS.md` as nearest prior art.
* Replace “FM oracle” with advisory semantic reviewer.
* Correct the claim that mutation-generated labels are free.
* Add the current METR qualification.
* Replace categorical “no incumbent” language with a narrower market claim.
* Define `ChangeIntent`, `ObservationSpec`, and witness validity.
* Stop using “proof” or “behavior-preserving” as a system output.

## Step 1 — Build the evaluation corpus first

Create a balanced corpus containing:

* SemaDiff’s Java cases where usable.
* The Java refactoring-bug corpus, with compilation and behavioral cases reported separately.
* Verified preserving refactors.
* Verified breaking refactors.
* Curated non-equivalent mutants.
* Recent agent-generated diffs from one or more private repositories.
* Existing-test-caught and existing-test-missed strata.

The eval runner should exist before the production engine so that every architecture decision is measurable.

## Step 2 — Build a Go reference engine

Implement only:

* Core contracts.
* Baseline/candidate workspace materialization.
* Build and test adapters.
* VerificationTarget extraction.
* Native differential/property/fuzz execution.
* Witness replay and minimization.
* Evidence JSON.
* Content-addressed artifacts and execution manifests.

Do not build SENSE, a dashboard, macOS support, or agent-specific integrations yet.

## Step 3 — Replicate the strongest Java baselines

Build enough Java support to:

* Run the SemaDiff-style unchanged-caller approach.
* Compare against SafeRefactor and ordinary build/test baselines.
* Evaluate the FM reviewer honestly on behavioral versus compilation failures.
* Establish how much of the apparent signal is already captured by conventional tooling.

This is evaluation infrastructure, not a commitment to Java as the first commercial ecosystem.

## Step 4 — Select one design partner and one production language

The design partner should have:

* Heavy agent use.
* Mature CI.
* Frequent refactoring or migrations.
* Enough deterministic service or library logic.
* Willing reviewers who can adjudicate findings.
* Historical merged, reverted, and regression-causing PRs.

Choose TypeScript, Python, or Go based on that repository. Do not choose the customer to validate a predetermined language roadmap.

## Step 5 — Historical replay

Run against historical PRs without affecting CI:

* Known good preserving changes.
* Known regressions.
* Reverted PRs.
* Changes with review debates.
* Agent-generated PRs.

This reveals eligibility, infrastructure friction, and observation problems before live reviewer behavior contaminates the evaluation.

## Step 6 — Shadow-mode CI

Publish evidence without blocking:

* What ran.
* What was unsupported.
* Observation coverage.
* Candidate divergences.
* Validated witnesses.
* Reproduction artifacts.

Measure whether reviewers open, replay, dismiss, or act on the output.

## Step 7 — Selective blocking

Initially block only:

* Build or typecheck regression.
* Existing-test regression.
* New high-confidence static/security regression.
* A replayed, domain-valid, contract-relevant behavioral witness.

FM opinions, generated-test failures without independent validation, low-confidence divergences, and unsupported targets remain advisory.

## Step 8 — Add SENSE only if it demonstrates lift

After PROVE is useful, compare:

* Unscoped agent proposal and implementation.
* Detector-scoped proposal.
* Detector-scoped, propose-then-rank.
* Human-selected target.

SENSE earns investment only if it improves accepted change rate, reduces cost, or produces changes with better verification eligibility.

## Step 9 — Expand languages conditionally

Expand only after:

* A design partner demonstrates reviewer or defect-prevention value.
* Synthesized harnesses approach native techniques on the chosen benchmark.
* The evidence contracts survive a second materially different ecosystem.
* Customers request the additional language.

Swift should be last. Designing honest degraded support is useful; implementing the macOS execution substrate before product validation is not.

---

# How to interpret the experiment’s outcomes

## High precision, low eligibility

This can still be a viable product, but as a narrow high-assurance gate for particular change classes. Do not dilute precision merely to claim wider coverage.

## High eligibility, excessive irrelevant divergences

Keep it advisory. Invest in observation contracts, precondition inference, comparators, and stable callers—not more fuzzing volume.

## Good seeded-mutation results, poor real-regression results

The eval has overfit to mutation operators. Replace generic mutants with project-history-derived and agent-derived fault models.

## Strong defect detection, no reviewer-time improvement

It may be valuable as internal quality infrastructure or an offline release gate, but the review-throughput product thesis is unsupported.

## Strong review impact, weak automated blocking precision

Sell evidence assistance rather than merge enforcement. A product does not need to block to be valuable.

## Synthesized harnesses materially trail native fuzzing

Keep language-specific native adapters and narrow the language promise. Do not let portability force the product toward its weakest evidence rung.

## FM reviewer does not improve routing economics

Remove it. A seven-rung system is not inherently better than a four-component system.

---

# Final recommendation

**Build PROVE-Lab, not the full Simpleton platform.**

The first release should have:

* One reference language.
* One real design-partner repository.
* Explicit behavior-preservation intent.
* Stable observation boundaries.
* Native differential execution.
* Replayable behavioral witnesses.
* Honest unsupported and inconclusive outcomes.
* Shadow-mode CI evidence.
* A balanced evaluation corpus.

Do not initially build:

* Four-language parity.
* Swift/macOS execution.
* A generic review UI.
* A custom structural index.
* A broad technical-debt backlog.
* Trend dashboards.
* Automated agent planning.
* An FM correctness gate.
* Autonomous merge promotion.

The strategic thesis survives the deeper review: **executable evidence is a better place to invest than another code-generating agent.** The opportunity, however, is not “prove arbitrary diffs are behavior-preserving.” It is the narrower and more defensible problem of **finding semantic regressions that ordinary tests miss, producing a witness reviewers can replay, and being explicit about everything the system did not establish.**

[1]: https://arxiv.org/html/2607.13111 "https://arxiv.org/html/2607.13111"
[2]: https://arxiv.org/html/2605.02096 "https://arxiv.org/html/2605.02096"
[3]: https://arxiv.org/html/2602.15761 "https://arxiv.org/html/2602.15761"
[4]: https://arxiv.org/html/2603.00520 "https://arxiv.org/html/2603.00520"
[5]: https://pitest.org/quickstart/basic_concepts/ "https://pitest.org/quickstart/basic_concepts/"
[6]: https://www.conference-publishing.com/toc/FSE26/abs "https://www.conference-publishing.com/toc/FSE26/abs"
[7]: https://www.sonarsource.com/company/press-releases/sonar-acquires-gitar/ "https://www.sonarsource.com/company/press-releases/sonar-acquires-gitar/"
[8]: https://arxiv.org/abs/2606.13468 "https://arxiv.org/abs/2606.13468"
[9]: https://metr.org/blog/2026-02-24-uplift-update/ "https://metr.org/blog/2026-02-24-uplift-update/"
