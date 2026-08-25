# Simpleton — Deterministic Scaffolding for Agentic Code Simplification

*Design document. Revised 2026-08-04 against the evidence record in `FINDINGS.md`. Supersedes the original plan, which was written from `research/` before its claims were verified. Status: design, no code yet.*

---

## 1. Thesis

The research is convergent on a narrow point: **autonomous LLM refactoring does not work, but constrained, verified pipelines do.** The original plan drew the right conclusion and then built the wrong thing — a full seven-stage pipeline that would have competed head-on with Moderne, Codemod.com and Grit.io on transformation quality, which is the one part of the problem that a well-funded incumbent already solves and that improves for free as models improve.

The revision inverts the build/rent boundary:

> **Simpleton owns the deterministic scaffolding — what to look at, and whether the change was safe. It rents planning and editing from a commodity agent harness.**

Concretely: Simpleton is **SENSE**, **PROVE** and **LEARN**. It does not write code.

This is a bet on durability. Detector evidence, verification verdicts, counterexamples, eval sets and trend series survive model generations and harness churn. Transformation quality does not — it is the thing most likely to be commoditised out from under any investment made in it today.

It is also a bet on where the value actually is. The binding constraint on AI-assisted maintenance is not generation, it is **review throughput**: 46.41% of agent-authored fixes are rejected, one of the four named rejection categories is *low priority*, larger diffs merge less often, and a controlled trial found experienced developers 19% slower on repositories they knew well (`FINDINGS.md` §2). A verification layer that turns a diff into reviewable evidence attacks the actual bottleneck. A transformation layer adds to it.

---

## 2. Design principles

Each traceable to `FINDINGS.md`. Principles the original plan got wrong are marked **[revised]**.

1. **Detectors scope the model; they do not replace it. [revised]** The original plan had the LLM triage detector output and never propose. But the same study that found ChatGPT identifying only 28/180 opportunities also found that explaining the subcategory and narrowing the search space raised its success rate from 15.6% to 86.7%. Detection's job is to hand the model a *category and a region*, then let it propose within that frame, then rank. (§3a)

2. **Every oracle is probabilistic, including the deterministic engines. [revised]** 226 real refactoring bugs have been collected from IntelliJ, Eclipse and NetBeans across 47 refactoring types. "The IDE applied it" is evidence, not proof. No rung of the verification ladder may be treated as complete. (§3b)

3. **Tests are necessary and demonstrably insufficient.** Two methodologically unrelated studies converge on ~20% of semantic breaks escaping existing test suites — 21% via differential fuzzing at function level, 19.71% via adversarial test strengthening at repository level. This convergence is Simpleton's empirical keystone. (§2)

4. **Emit counterexamples, not verdicts.** Because the editing loop is rented, a verdict of "not equivalent" is unactionable. `x=[] → None before, [] after, at line 42` is a repair signal. This ranks the verification ladder by *counterexample productivity*, not only by cost.

5. **Verification is a hook, not a tool.** A verification step the agent may choose to call will be skipped under pressure. Enforcement must be structural.

6. **Hardening is change-scoped, not module-scoped. [revised]** The original plan hardened whole modules ahead of time — expensive and speculative, since it hardens modules that may never be touched. Strengthen tests for the *changed region* at verification time instead, per the SWE-ABS pattern: coverage-driven augmentation via program slicing, plus mutation-driven adversarial tests.

7. **One intent per diff.** Justified by build-greenness and reviewability, **not** by correctness — tangled refactorings show no significant association with functional correctness. The honest claim is compilability 19.34% → 38.33%. (§3c)

8. **Measure the system, not the function**, with **in-house frozen metric definitions**. GitClear's direction is credible and its editions re-baseline against each other; never wire autonomy to a vendor's shifting definitions. (§3e)

9. **Autonomy is earned per risk tier and revocable.** Unchanged from the original plan, and still right.

10. **Everything is an experiment.** Frozen prompts and model versions, stored traces, cost per accepted change, revert rate. Unchanged.

---

## 3. Architecture

Three owned components wrapped around a rented harness. `HARDEN` and `SHIP` from the original seven-stage design do not disappear — they fold into `PROVE` (see §3.2).

```
        ┌──────────────────────── Simpleton (owned) ────────────────────────┐
        │                                                                   │
        │   ┌─────────┐                                     ┌─────────┐     │
        │   │ 1 SENSE │                                     │ 3 PROVE │     │
        │   │ scope   │                                     │ verify  │     │
        │   └────┬────┘                                     └────┬────┘     │
        │        │                                               │          │
        └────────┼───────────────────────────────────────────────┼──────────┘
                 │                                               │
                 ▼                                               ▲
          ┌──────────────────────────────────────────────────────┴───┐
          │  2  AGENT HARNESS (rented)   plan → edit → self-repair    │
          │     Claude Code skills / subagents / any agent            │
          └───────────────────────────────────────────────────────────┘
                 │                                               │
                 └───────────────────┬───────────────────────────┘
                                     ▼
                            ┌─────────────────┐
                            │    4  LEARN     │   evals + trend series
                            └─────────────────┘
```

### 3.1 SENSE — narrow the search space

**Job:** hand the agent a category, a region, and evidence. Not a backlog for humans to read.

- **Detectors (deterministic, off-the-shelf):** duplication, dead code and unused exports, complexity hotspots, dependency cycles, oversized units, unused dependencies, policy violations, stale flags, coverage gaps.
- **Output — a *scoping bundle* per finding:** location, refactoring category, detector evidence, current coverage, risk tier, and the file set the agent may touch. This is a prompt input, not a report.
- **The model's role:** propose *within* the scoped category, N candidates, ranked — the propose-then-implement decomposition that CodeTaste found improves alignment with human refactoring choices.

**Success criterion — changed from the original plan.** Not "maintainers agree ≥70% of top-10 items are worth doing" (that evaluates a backlog product). The question is: **does scoping raise the agent's success rate versus unscoped?** Directly measurable, and it maps onto the 15.6% → 86.7% result that justifies the component at all.

**Deliberately not built yet:** the tree-sitter index and call graph. Off-the-shelf detectors (ruff, semgrep, jscpd, ts-prune, radon/lizard, madge) go a long way; the work is the composition and the evidence bundle. Build the index when the composition demonstrably needs it. Note also that structural anchoring's measured benefit is small (+2.2pp localisation) and mostly about *reproducibility* — halved run-to-run variance — which is worth having but does not justify heavy indexing infrastructure up front.

### 3.2 PROVE — the verification stack

**Job:** given a diff from *any* author, return evidence and counterexamples.

Ordered by cost and counterexample productivity. Every rung is probabilistic; none is a proof.

| # | Rung | Produces | Cost |
|---|---|---|---|
| 1 | Build / compile / type check | Compiler diagnostics | trivial |
| 2 | Existing test suite | Failing test + trace | low |
| 3 | Structural diff classification | *Which* refactoring is this? Is it tangled? | low |
| 4 | Static-analysis & security delta | New findings | low |
| 5 | **Foundation-model oracle** | Suspicion + explanation — **not** a counterexample | low |
| 6 | **Change-scoped test strengthening** | New failing test | medium |
| 7 | **Differential fuzzing / property probes** | Concrete counterexample input | high |

Notes on the rungs the original plan lacked or mis-ranked:

- **Rung 5** comes from the 226-bug IDE corpus: zero-shot foundation-model detection of refactoring bugs reaches 80.5–93.8% accuracy with no infrastructure and no refactoring-specific rules. Because it yields judgments rather than counterexamples, it is a **triage** rung — cheap suspicion that routes work to rungs 6 and 7 — never a gate on its own.
- **Rung 6** is the folded-in HARDEN stage, implemented per SWE-ABS: coverage-driven augmentation using program slicing to target untested regions in the diff, plus mutation-driven adversarial tests that synthesise plausible-but-incorrect patches to expose blind spots. Additive-only, per TestGen-LLM's assured-improvement design.
- **Rung 7** is the strongest evidence and the narrowest applicability. It reaches deterministic, isolatable, side-effect-free units only. **State the coverage honestly**: report what fraction of changed functions were reachable, and never describe the output as "behaviour-preserving" — the claim is *"no counterexample found at budget N."*

**Output — the audit pack.** This is the folded-in SHIP stage. Not a separate pipeline stage but the rendering of PROVE's evidence: what transformed and why, per-rung results, coverage delta, counterexamples found, metric deltas, executor provenance, trace link, rollback. Given that review throughput is the binding constraint, this artifact *is* the product surface. Review minutes per change is a primary metric, not a side metric.

**Enforcement.** PROVE runs twice: as a blocking hook inside the harness (structural, not optional), and as an independent CI gate. The CI path is what makes it work on diffs Simpleton did not generate — the property that makes the component valuable regardless of whether the rest of the system succeeds.

### 3.3 LEARN — evals and trends

Reduced from the original plan. The **codified three-tier memory is cut entirely** — `CLAUDE.md`, skills and subagents already are that infrastructure, and rebuilding it contradicts the decision to rent the harness.

What remains:

- **Eval harness (load-bearing, see §5).**
- **Trend series** on in-house frozen definitions: duplication, copy/paste vs moved lines, two-week churn, complexity distribution, plus three signals adopted from GitClear's 2026 edition — **cross-file function calls**, **legacy maintenance rate**, **error-masking construct density**. Advisory only at first; it gates nothing automatically until the definitions have a track record.
- **Outcome ledger:** acceptance rate, revert rate, cost per accepted change, review minutes per change.

---

## 4. The autonomy ladder

Revised: Tier 1's oracle is corrected, and every tier now names a real oracle rather than an assumed guarantee.

| Tier | Transformations | Oracle | Human gate | Promotion threshold |
|---|---|---|---|---|
| **0 — Mechanical** | formatting, import sorting, unused-import removal, unreferenced dead-file deletion | compiler + linter (near-complete for this class) | batch review → earned auto-merge | >99% acceptance over N, zero reverts |
| **1 — Tool-safe** | rename, extract, inline, move | rungs 1–5. **Not "engine guarantees"** — engines have 226 documented behaviour bugs across 47 refactoring types | per-PR, lightweight | >95% acceptance, zero behaviour regressions |
| **2 — Behaviour-preserving structural** | dedup, conditional simplification, dependency untangling | rungs 1–7 including change-scoped strengthening and differential fuzzing | per-PR, standard | >90% acceptance, counterexample rate trending down |
| **3 — Architectural** | module consolidation, API redesign, migrations | full stack + plan review before execution | human approves plan and every PR | stays human-led indefinitely |

---

## 5. Evaluation

**The primary metric is self-supervised and costs nothing to label.** Seed known semantics-breaking mutations into real code — exactly what `mutmut`, `cosmic-ray`, `Stryker` and `PIT` already do — and measure PROVE's catch rate, stratified by whether the existing test suite catches it:

|  | Test suite catches | Test suite misses |
|---|---|---|
| **PROVE catches** | redundant (fine) | **← the entire value proposition** |
| **PROVE misses** | acceptable | false confidence — the failure mode to minimise |

The bottom-left/top-right cells are the whole product, quantified, on any repository, with no human in the loop. The original plan had this as a Phase 2 exit criterion; it should be the primary metric from week one, because it is the only thing that says whether the hardest technical bet is real.

**Secondary:** SENSE's scoping lift (agent success rate scoped vs unscoped). **Public benchmarks:** SWE-Refactor and CodeTaste — refactoring-specific, post-dating most contamination, scored by static checks plus tests. **Not SWE-bench**: 32.67% solution leakage, 31.08% weak tests, and one in five "solved" Verified patches semantically incorrect.

---

## 6. Build order

1. **PROVE on diffs Simpleton did not generate.** CLI + CI gate. Needs no SENSE and no agent. Validates the hardest bet: can rung 7 produce counterexamples on real code, and at what coverage?
2. **Seeded-mutation eval harness**, concurrently — step 1 is unmeasurable without it.
3. **PROVE as a blocking hook** in the harness, closing the loop via counterexample feedback to the rented agent's self-repair.
4. **SENSE as scoper**, off-the-shelf detectors only, evaluated by scoping lift.
5. **Trend series and structural index** only if 1–4 demand them.

**Portability.** Core is a language-agnostic CLI emitting JSON. Thin adapters: MCP server for SENSE queries, hooks for PROVE enforcement, CI action for the gate. Harness-native is then a *deployment* choice, not an architecture commitment.

---

## 7. Language choice

**Python and TypeScript first. Java as evaluation corpus only.**

Renting the transformation layer removes the main argument for Java — the deterministic-engine ecosystem (OpenRewrite, mature IDE refactoring APIs) only matters if you are applying transformations. For a verification product the requirements invert: strong fuzz and property-testing infrastructure, and high volume of agent-authored code needing review. Hypothesis is the best property-testing library in any ecosystem; `mutmut`/`cosmic-ray` and `Stryker` cover mutation; Python and TypeScript are what agents write most.

Java stays as the eval corpus because SWE-Refactor (1,099 instances, 18 projects) and the 226-bug refactoring-oracle corpus are both Java, as is RefactoringMiner.

---

## 8. What Simpleton deliberately is not

- **Not a transformation engine.** Moderne serialises LSTs across an estate and exposes OpenRewrite recipes as deterministic MCP tool calls to agents; Codemod.com generates ast-grep rules from natural language and orchestrates org-wide migrations; Grit.io ships AST transforms as auto-generated PRs. Competing here means competing on the part that improves for free as models improve.
- **Not a technical-debt backlog product.** That market is occupied (Codemod's migration insights, CodeScene, Sonar) and the AIDev data shows "low priority" is a top rejection reason for correct, verified work. SENSE exists to scope an agent, not to produce a report.
- **Not an autonomous agent given a repo and "make it better."** Worst-evidenced configuration in the corpus.
- **Not multi-agent-first.** Agentless: 32.00% at $0.70 beat elaborate agent scaffolds.
- **Not a rewriter.** Ship-of-Theseus module rewrites stay out of scope and human-led.
- **Not benchmark-driven.** Acceptance rate, revert rate, review minutes, seeded-mutation catch rate, and codebase trend lines on real repos.

---

## 9. Open questions

1. **What is rung 7's real coverage on production code?** The 19–35% non-equivalence band comes from function-level benchmarks (HumanEval/MBPP/APPS) using non-frontier models. Side effects, I/O and global state will exclude a large share of real changed functions. This is the single biggest unknown and step 1 of the build order exists to answer it.
2. **Does rung 5 hold outside its corpus?** The 93.8% figure is zero-shot on a decade-old collection of IDE bugs; the authors explicitly decline to rule out memorisation or contamination. Needs replication on the seeded-mutation eval before it earns a place in the ladder.
3. **Does scoping lift survive frontier models?** The 15.6% → 86.7% result predates current models. If frontier agents no longer need scoping, SENSE's justification weakens and the system reduces to PROVE + LEARN — which is an acceptable outcome, but should be discovered deliberately.
4. **Counterexample quality as a repair signal.** Does a rented agent actually self-repair better from a concrete counterexample than from a failing test name? Cheap to test, and principle 4 depends on it.
5. **Pilot codebase.** Step 1 needs a real repository with real merged diffs to verify against. Simpleton's own repo is too small; an internal or well-chosen OSS project with strong CI is needed.
6. **Is this a product or internal tooling?** Affects only packaging (portable CLI vs harness-specific), not architecture — §6 keeps both open.

---

## 10. Research traceability

Full evidence record, including verbatim-verified figures, corrections and unverified claims, is in `FINDINGS.md`.

| Design element | Grounding |
|---|---|
| Rent transformation, own verification | Moderne / Codemod / Grit occupy transformation; nothing occupies verification (§6) |
| Verification stack is the keystone | Differential fuzzing ~21% + SWE-ABS 19.71% converging independently (§2) |
| Detectors scope rather than replace | ChatGPT 15.6% → 86.7% when scoped; RefAgent F1 79.15%; CodeTaste propose-then-implement (§3a) |
| Foundation-model oracle rung | 226 IDE refactoring bugs, 47 types, 80.5–93.8% zero-shot detection (§2, §3b) |
| Tier 1 oracle correction | Same corpus — IDE engines introduce behaviour changes (§3b) |
| Change-scoped hardening | SWE-ABS coverage-driven slicing + mutation-driven adversarial tests (§2); TestGen-LLM additive-only design |
| Counterexamples over verdicts | Consequence of renting the editing loop |
| Audit pack as product surface | 46.41% agent-PR rejection; "low priority" a named category; larger diffs merge less; METR −19% (§2) |
| One intent per diff (compilability only) | Tangling 19.34% → 38.33%, *no* significant correctness association (§3c) |
| Seeded-mutation eval | Mutation tooling already generates the labels; original plan's Phase 2 criterion promoted to primary |
| Pipeline over swarm | Agentless 32.00% at $0.70 |
| In-house frozen trend metrics | GitClear re-baselining between editions (§3e); 2026 edition's new signals adopted |
| Python/TS first, Java as corpus | Renting transformation removes the deterministic-engine argument (§7 implications) |
| SWE-Refactor / CodeTaste over SWE-bench | 32.67% leakage, 31.08% weak tests, 1-in-5 Verified patches wrong (§1, §2) |
