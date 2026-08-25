# Implementation — Architecture and Practical Approaches

*Companion to `PLAN.md` (design) and `FINDINGS.md` (evidence). Written 2026-08-04. Scope: how to actually build SENSE, PROVE and LEARN across TypeScript, Python, Go and Swift.*

---

## 1. What multi-language actually costs

The naive read is that four languages means four times the work. It doesn't, and the way it doesn't is the single most important fact for the architecture:

> **Per-language cost is roughly *inverse* to the rung's evidence strength.**

The foundation-model oracle (rung 5) — the weakest evidence, judgment-only, no counterexample — is **free in every language**. Differential fuzzing (rung 7) — the strongest evidence, produces real counterexamples — is the most expensive to port and the hardest in Swift by a wide margin.

Left unmanaged, multi-language support therefore exerts constant downward pressure toward weaker oracles. A system that quietly ran rung 5 everywhere and rung 7 nowhere would look like it had broad coverage while proving almost nothing. Two design commitments follow, and everything else in this document serves them:

1. **Capability is declared, never assumed.** Every language pack publishes a manifest of which rungs it can run and under what conditions. A rung that cannot run returns `unsupported` with a reason — never silently skipped, never faked.
2. **Every verdict carries its evidence profile.** The output is not "safe/unsafe" but *which rungs ran, at what budget, over what fraction of the changed units*. Eligibility rate is a first-class metric, reported per diff and tracked over time.

There is one more structural fact worth stating up front: **Swift is the forcing function.** If you build TypeScript and Python first, you will bake in assumptions — dynamic module loading, sub-second builds, Linux sandboxing — that Swift violates on all three counts. Designing the capability contract so Swift works honestly at rungs 1–6 and is *openly degraded* at rung 7 produces an architecture that is correct for everything else too.

---

## 2. Capability reality

Concrete tooling per rung. This table is the substance of the design; the architecture is mostly a way of organising it.

### Verification rungs

| Rung | TypeScript | Python | Go | Swift |
|---|---|---|---|---|
| **1** Build / typecheck | `tsc --noEmit` — strong | `mypy` / `pyright` — **optional typing, partial oracle** | `go build`, `go vet` — strong | `swift build` — strong types, **slow** |
| **2** Tests | vitest/jest → JUnit XML | `pytest --junitxml` | `go test -json` | swift-testing / `xcodebuild` → xcresult (**parsing friction**) |
| **3** Structural diff | tree-sitter + query pack | tree-sitter + query pack | tree-sitter, or `go/ast` + `go/types` for precision | tree-sitter, or SwiftSyntax |
| **4** Static-analysis delta | eslint / oxlint + semgrep → SARIF | ruff + semgrep → SARIF | staticcheck / vet + semgrep → SARIF | SwiftLint + semgrep → SARIF |
| **5** FM oracle | **free** | **free** | **free** | **free** |
| **6** Test strengthening | **Stryker** (native `--incremental`) + c8 → lcov | mutmut / cosmic-ray + coverage.py → lcov | Gremlins (*"smallish modules"*) + `go test -cover` | **Muter** (SwiftSyntax; documented iOS-scale use) + llvm-cov / xccov |
| **7** Differential fuzz | fast-check; twins easy | **Hypothesis** (best-in-class shrinking) + atheris; **twins trivial** | **`go test -fuzz`** native + **gosentry** (LibAFL, struct-aware, Nautilus grammars) | FuzzCheck (**experimental**); twins hard, builds slow |

Two observations. First, **all four languages have a working mutation tool** — better than expected, and it enables both rung 6 and the seeded-mutation eval harness (`PLAN.md` §5) everywhere. Second, **Go is the strongest rung-7 language available**, not Python: native coverage-guided fuzzing in the standard toolchain, plus Trail of Bits' 2026 `gosentry` fork that swaps in LibAFL, adds native struct fuzzing and grammar-based fuzzing, and emits campaign coverage in one command.

### Semantic index

| | SCIP indexer | LSP | Native |
|---|---|---|---|
| TypeScript | `scip-typescript` | tsserver | ts-morph |
| Python | `scip-python` | pyright | `ast`, libcst |
| Go | `scip-go` | gopls | **`go/ast` + `go/types`** — full type info in stdlib |
| Swift | **none** | SourceKit-LSP | **IndexStoreDB** (LMDB, fed by `swift-frontend -index-store-path`) |

SCIP is a language-agnostic protocol whose consumers "do not need language-specific knowledge" — exactly the property we want — but the indexer ecosystem has a **Swift-shaped hole**. Swift's equivalent is IndexStoreDB, queried directly or via SourceKit-LSP.

### Detectors for SENSE

| Signal | Tool |
|---|---|
| Duplication | **jscpd** — Rabin-Karp, 150+ languages, covers all four in one tool (PMD CPD covers 31) |
| Dead code | knip (TS, module-graph, ~150 plugins) · vulture (Py) · `x/tools/cmd/deadcode` (Go, official) · **Periphery** (Swift) |
| Complexity | lizard (multi-language) or per-language |
| Policy | semgrep (one rule format, all four) |

SENSE is almost entirely off-the-shelf, which is what `PLAN.md` §3.1 assumed. The work is composition and the evidence bundle, not detection.

---

## 3. Architecture

Four layers. Only layer 2 is per-language, and it is deliberately thin.

```
┌─ L0  CONTRACTS ──────────────────────────────────────────────────┐
│  JSON schemas: ScopeBundle · ChangedUnit · RungResult ·          │
│  Counterexample · Verdict · CapabilityManifest                   │
│  Language-neutral. This is the actual product.                   │
├─ L1  ENGINE ─────────────────────────────────────────────────────┤
│  Ecosystem routing · capability negotiation · rung sequencing ·   │
│  budgets · content-addressed cache · adjudication · audit pack ·  │
│  eval harness. Language-neutral.                                  │
├─ L2  LANGUAGE PACKS ─────────────────────────────────────────────┤
│  ts/ · py/ · go/ · swift/                                        │
│  Each: capability manifest + tool adapters + tree-sitter query    │
│  pack + twin-driver template + eligibility screen.               │
│  Data and small adapters — not forks.                            │
├─ L3  EXECUTION SUBSTRATE ────────────────────────────────────────┤
│  Sandbox + toolchain images. Linux microVM for TS/Py/Go;          │
│  macOS runner + process isolation for Swift (see §6).            │
└──────────────────────────────────────────────────────────────────┘
```

### The three contracts that matter

**`ChangedUnit`** — the atom of verification. `{language, file, symbol, kind, pre_blob, post_blob, eligibility: {fuzzable, reason}}`. Extracting these from a diff is the main job of a language pack's tree-sitter query pack.

**`Counterexample`** — `{unit, input, pre_output, post_output, replay_command}`. Must be independently replayable. §5 explains why this single requirement carries most of the architecture.

**`Verdict`** — never a boolean. `{units_total, units_eligible, rungs: [{id, status: ran|unsupported|budget_exhausted, reason, budget, findings}], counterexamples: [...]}`. "No counterexample found at budget N" with N present, or it is not a verdict.

### Normalising formats instead of building them

Every cross-language format problem already has an answer. Use it rather than inventing one.

- **SARIF** for static-analysis findings — every analyzer emits it; rung 4 is a set-difference over SARIF.
- **LCOV** for coverage — `grcov` normalises `.profraw` / `.gcda` / lcov / JaCoCo; Go and coverage.py convert cleanly. Swift goes llvm-cov → lcov.
- **JUnit XML** for test results — all four ecosystems emit it (Swift via xcresult conversion, the one lossy path).
- **SCIP** for semantic facts where an indexer exists; LSP where it doesn't; tree-sitter as the floor.

---

## 4. Five approaches to the analysis layer

The real architectural choice is how to get structural and semantic facts across four languages. These are the genuine alternatives.

### Approach A — Universal IR / common AST (the Semgrep model)

Map every language into one abstract syntax tree; write each analysis once.

**For:** analyses written once, truly language-agnostic, extensible by adding a front end.
**Against:** lossy by construction — Semgrep is "loosely coupled with each language," which is precisely why it is confined to pattern matching. You lose the type information that makes rung 3 meaningful. Swift's optionals, protocol witnesses, ARC semantics and value/reference distinction do not survive the mapping. And it is a very large up-front build.
**Verdict: reject.** The cost lands before any value, and the lossiness bites exactly where we need precision.

### Approach B — Tree-sitter monoculture

One parser, 306+ grammars including Swift, per-language query packs for the rest.

**For:** single API, error-tolerant, fast, usable as a **library** (Semgrep is not), grammars already exist for all four, handles partial/broken code — which matters because we parse both sides of a diff.
**Against:** CST not AST — no types, no cross-file resolution, cannot tell you the receiver type of `foo.bar()`. Grammar quality varies.
**Verdict: adopt as the floor.** Necessary, not sufficient.

### Approach C — Native toolchain shell-out behind a JSON contract

Each pack shells out to the ecosystem's own tools; the engine only orchestrates and normalises.

**For:** maximum fidelity; you inherit ecosystem quality for free and it improves without you; per-language code stays tiny; `go/types` and SwiftSyntax give you things no universal layer can.
**Against:** N toolchains to install, version, pin and sandbox; heterogeneous failure modes; Swift build latency is a real tax on any loop that rebuilds.
**Verdict: adopt for rungs 1, 2, 4, 6, 7.** This is where the leverage is.

### Approach D — LSP as the universal semantic API

Every ecosystem ships a language server that speaks one protocol: gopls, pyright, tsserver, SourceKit-LSP.

**For:** one protocol, real semantics, no indexer to build, and it is the *only* uniform semantic option for Swift.
**Against:** built for interactive editing — stateful, slow to warm, flaky under batch load, no stable bulk API. SourceKit-LSP needs a built project, which reintroduces Swift build cost.
**Verdict: adopt as the middle tier, used sparingly and cached hard.** Never on the hot path.

### Approach E — Capability-tiered hybrid **(recommended)**

```
tree-sitter  ─── always available, syntactic floor
     ↓ escalate only when the rung needs semantics
LSP / SCIP   ─── usually available, cached, cross-file resolution
     ↓ escalate only when precision is decisive
native tools ─── go/types, SwiftSyntax, ts-morph, libcst
```

Each language pack declares which tier it reaches for which capability. The engine plans the cheapest tier that satisfies the rung and records which tier actually ran, so a Swift verdict and a Go verdict are comparable *and* visibly different.

**The cost of E** is that the engine must express "this rung ran at tier 1 in Swift and tier 3 in Go" without collapsing the distinction — the reporting complexity is real and permanent. That is the price of honesty, and it is cheaper than either the universal-IR build or the pretence that all languages are equally covered.

---

## 5. The rung-7 decision: synthesised harnesses over ported fuzzers

Rung 7 needs a **twin harness**: extract a changed function's before and after versions, build both, drive them with identical inputs, compare. Cost varies enormously.

| | Twin construction | Difficulty |
|---|---|---|
| Python | import both under different `sys.path` entries | trivial |
| TypeScript | two builds, or module aliasing | easy |
| Go | two modules with distinct paths, or `replace` directives | moderate — package-level compilation is the friction |
| Swift | two static libraries + driver executable, module namespacing, slow rebuilds | hard |

The obvious plan — port a native fuzzing stack per language — costs the most exactly where it is hardest, and would leave Swift with nothing for a long time.

**The alternative: have the model synthesise the differential harness per diff, and let the engine merely run it in a sandbox and adjudicate the result.**

The objection is immediate: the harness is now LLM-generated and unverified, which is the failure mode this whole system exists to guard against. The answer is an asymmetry that makes the objection dissolve:

> **You never need to trust the harness. You only need to trust the counterexample — and a counterexample validates itself by replay.**
>
> - A **false** counterexample is trivially killed: re-run both versions on the claimed input under a fixed seed. If outputs match, discard.
> - A **missed** counterexample is a coverage loss, not a safety loss — and it is already accounted for by reporting eligibility rate.

The error mode of a bad harness is therefore *silence*, which the evidence profile already exposes, not *false assurance*, which would be fatal. This is the same logic as TestGen-LLM's assured-improvement filter: let an unreliable generator propose, and gate on a mechanical check.

The evidence supports the generator being good enough:

- **Cleverest** — LLM-generated, feedback-directed regression tests targeting a specific code change found **as many bugs in under 2 minutes as the state-of-the-art directed greybox fuzzer WAFLGo found in 24 hours**, across 72 commits to Mujs, Libxml2, Poppler, JerryScript, Z3, PHP, JQ and MicroPython. Seeding coverage-guided fuzzing with those tests **doubled** the bug count ([2501.11086](https://arxiv.org/abs/2501.11086)).
- **HarnessLLM** — training models to write *harness code* (synthesising inputs and validating outputs, enabling invariant checks) beats input/output-pair generation on bug finding and strategy diversity ([2511.01104](https://arxiv.org/abs/2511.01104)).
- **AutoHarness** — iterative harness synthesis from environment feedback, with a small model plus harness outperforming a much larger model alone ([2603.03329](https://arxiv.org/abs/2603.03329)).

Cleverest matters most: it is *change-directed* — it takes the commit and the diff, which is precisely our input — and its 2-minutes-versus-24-hours result is the difference between a rung that fits in CI and one that does not.

**Resulting design.** Harness synthesis is the **primary, portable** rung-7 strategy. Native fuzzers are **accelerators where they exist**, not prerequisites:

- **Go** — synthesised harness seeds `go test -fuzz` (and gosentry/LibAFL where installed). Best of both.
- **Python** — synthesised harness expressed as a Hypothesis property, inheriting its shrinking.
- **TypeScript** — synthesised harness expressed as a fast-check property.
- **Swift** — synthesised harness compiled as an SPM test target. **This is how Swift gets rung 7 at all**, without building a Swift fuzzing stack.

Non-negotiable guards, because the generator is untrusted:
1. The harness must **compile**.
2. It must **pass on the pre-image** — a harness that fails before the change is measuring its own bug.
3. Fixed seeds; every counterexample ships a `replay_command`.
4. Automatic replay before any counterexample is reported.
5. Harnesses are **cached and versioned** by unit — they are reusable assets, not per-run throwaways.

---

## 6. Execution substrate, and the Swift problem

Rungs 2, 6 and 7 execute code that an agent may have written. The 2026 consensus for untrusted execution is microVM isolation — Firecracker boots in ~125 ms with under 5 MiB overhead — with gVisor as the Kubernetes-native alternative.

**That story does not cover Swift.** Firecracker and gVisor are Linux. Server-side Swift on Linux is fine; anything touching Apple platform frameworks is not. Swift verification needs **macOS runners with process-level isolation** — weaker containment, scarcer and more expensive hardware, and no microVM boot-time trick.

This is not a detail to discover later. It means:

- The substrate is **pluggable per ecosystem**, decided by the language pack's manifest, not global.
- Swift's isolation tier is **recorded in the verdict** alongside its rung coverage — a Swift verdict is produced under materially different containment from a Go one, and pretending otherwise is the kind of quiet dishonesty this design exists to prevent.
- Swift build latency compounds the problem. Mitigations: restrict to SPM targets rather than `xcodebuild` wherever possible, warm module caches, persist derived data across runs, and cache aggressively by content hash.

---

## 7. Cross-cutting mechanics

**Ecosystem routing.** Detect by manifest — `package.json`/`tsconfig.json`, `pyproject.toml`/`setup.py`, `go.mod`, `Package.swift`/`*.xcodeproj`. Map each changed file to an ecosystem; a diff spanning several runs several packs and merges verdicts. Polyglot monorepos are the normal case, not the exception.

**Eligibility screening.** Rung 7 cannot touch code that does I/O or is nondeterministic. Two-stage: a static screen per language (no `io`/`net`/`time`/`random` imports, no global writes, no unsafe), then runtime confirmation — run twice on identical input and compare, with the sandbox trapping syscalls. **Report the eligibility rate.** It is the direct empirical answer to `PLAN.md` §9 Q1, and the number most likely to determine whether this product is viable.

**Caching.** Content-address everything by `(rung, adapter version, pre_blob, post_blob, config hash)`. Rungs 6 and 7 are expensive enough that the cache is load-bearing, not an optimisation. Stryker's `--incremental` mode is the precedent worth copying: track changes and re-mutate only what moved, which brings per-PR mutation testing into the 1–5 minute range.

**Budgets.** Per-rung wall-clock and token ceilings, recorded in the verdict. A rung that exhausts its budget returns `budget_exhausted`, which is a different verdict from `unsupported` and from `ran`.

**Flakiness.** The practical killer of any CI gate. Every execution rung runs twice on identical inputs before a failure is reported; disagreement is classified as flaky, not as a finding.

---

## 8. Build order

This revises `PLAN.md` §6 in one respect: **Go, not Python, is the first language pack.**

`PLAN.md` step 1 exists to answer "can rung 7 produce counterexamples on real code, and at what coverage?" Go answers that question faster and more cheaply than anything else available — native coverage-guided fuzzing, full type information in the standard library, fast hermetic builds, clean Linux sandboxing. Using it first is a change of language but not of intent.

1. **L0 contracts + L1 engine + capability manifest.** Language-neutral. No verification yet.
2. **Go pack, rungs 1–3 + 7.** Native fuzzing proves the hardest rung end-to-end at minimum cost. Deliverable: eligibility rate and counterexample yield on a real Go repository.
3. **Seeded-mutation eval harness** (Gremlins), concurrently — step 2 is unmeasurable without it.
4. **Python pack.** Hypothesis for shrinking, trivial twin construction, and the largest volume of agent-written code. Validates that the contracts survive a second, quite different ecosystem.
5. **Harness synthesis (§5) as the portable rung-7 path**, validated against Go's native fuzzing — where you have ground truth from step 2 to measure it against.
6. **TypeScript pack.** Stryker's incremental mode makes rung 6 strongest here.
7. **Swift pack, deliberately partial.** Rungs 1–6 plus synthesised property probes; macOS substrate; rung 7 openly degraded. Swift is the test of whether honest degradation actually works, so it must be attempted early enough to invalidate the design if it doesn't — but not before the contracts have survived two other ecosystems.
8. **SENSE** across all packs, off-the-shelf detectors only, evaluated by scoping lift.

**Validate the FM oracle per language.** The 93.8% figure comes from a Java-only corpus of IDE refactoring bugs, and its authors explicitly decline to rule out contamination. Because rung 5 is the one rung that is free everywhere, it is also the one most likely to be over-trusted. Measure it separately in each language against seeded mutations before granting it any weight.

---

## 9. Open questions

1. **What is rung-7 eligibility on real code?** Inherited from `PLAN.md` §9 Q1 and still the biggest unknown. Now sharper: measure it *per language*, since Python's dynamism, Go's explicit effects, TypeScript's async and Swift's ARC will give four different answers.
2. **Does harness synthesis match native fuzzing where both exist?** Step 5 of the build order is designed to answer this against Go ground truth. If it does not, Swift's rung 7 is not merely degraded but absent, and the four-language promise weakens.
3. **Does the FM oracle generalise beyond Java?** See §8.
4. **Is tree-sitter enough for rung 3?** The three judgments actually needed — structural vs functional vs tangled, changed-unit extraction, and did-the-declared-transformation-occur — look achievable on CST plus query packs. This is an assumption, not a result. Note that RefactoringMiner is "migrating to a multi-language support infrastructure," which may make part of this moot.
5. **Swift on macOS runners — cost and containment.** Weaker isolation and scarcer hardware may make Swift economically unattractive even where it is technically supported.
6. **Does `go/types`-grade precision beat tree-sitter enough to justify tier escalation?** Go is the cheapest place to run this experiment, and the answer generalises to how much LSP/SCIP work the other packs deserve.

---

## 10. Sources

Tooling and ecosystem claims in §2, §5 and §6 rest on:
[gosentry / Go fuzzing](https://blog.trailofbits.com/2026/05/12/go-fuzzing-was-missing-half-the-toolkit.-we-forked-the-toolchain-to-fix-it./) ·
[Gremlins](https://gremlins.dev/0.2/) ·
[Muter](https://github.com/muter-mutation-testing/muter) ·
[Scaling mutation testing in a large iOS codebase](https://ericsspace.com/articles/scaling-mutation-testing-in-a-large-ios-codebase/) ·
[FuzzCheck](https://github.com/loiclec/FuzzCheck) ·
[swift-testing](https://github.com/swiftlang/swift-testing) ·
[IndexStoreDB](https://github.com/swiftlang/indexstore-db) ·
[SourceKit-LSP semantic indexing](https://deepwiki.com/swiftlang/sourcekit-lsp/6-lsp-implementation) ·
[Periphery](https://github.com/peripheryapp/periphery) ·
[Stryker incremental mode](https://stryker-mutator.io/docs/stryker-js/incremental/) ·
[knip](https://recca0120.github.io/en/2026/05/02/knip-dead-code-detector/) ·
[cosmic-ray](https://github.com/sixty-north/cosmic-ray) ·
[SCIP](https://scip-code.org/) ·
[ast-grep vs Semgrep comparison](https://ast-grep.github.io/advanced/tool-comparison.html) ·
[tree-sitter parser list](https://github.com/tree-sitter/tree-sitter/wiki/List-of-parsers) ·
[jscpd](https://github.com/kucherenko/jscpd) ·
[grcov](https://github.com/mozilla/grcov) ·
[MicroVM isolation in 2026](https://emirb.github.io/blog/microvm-2026/) ·
[RefactoringMiner](https://github.com/tsantalis/RefactoringMiner) ·
[SemanticDiff](https://semanticdiff.com/)

Research claims in §5: [2501.11086](https://arxiv.org/abs/2501.11086) · [2511.01104](https://arxiv.org/abs/2511.01104) · [2603.03329](https://arxiv.org/abs/2603.03329).
