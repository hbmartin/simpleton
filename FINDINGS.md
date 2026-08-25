# Findings — Verification of the `research/` Claims

*Evidence record. Compiled 2026-08-04 by independent check of the claims in `research/` against primary sources (arXiv, vendor reports, product documentation). This document is the evidentiary basis for `PLAN.md`; where the two disagree, this document wins.*

**Method.** Every load-bearing number in the three `research/` documents was traced to a primary source and compared verbatim against the abstract or report text. Claims that could not be traced are listed in §5 rather than silently dropped. Competitive/product claims (§6) come from vendor documentation and are marked as such.

**Headline.** The research corpus is unusually accurate — nearly every quantitative claim reproduces verbatim. But three claims are *directionally right and mechanically wrong* in ways that change the design (§3), and the plan they produced had a significant blind spot on prior art (§6).

---

## 1. Claims verified verbatim

| Claim (as stated in `research/`) | Source | Status |
|---|---|---|
| EM-Assist: up to 76.3% of raw LLM suggestions are hallucinations; recall 53.4% top-5 vs 39.4% for best prior static-analysis tool; 94.4% positive rating from 18 industrial developers; 81.3% agreement from 16 developers | [2401.15298](https://arxiv.org/abs/2401.15298), [2405.20551](https://arxiv.org/abs/2405.20551) | ✅ verbatim |
| MANTRA: 82.8% (582/703) compiles + passes tests vs RawGPT 8.7% (61/703); ~50% improvement over EM-Assist on Extract Method; 703 pure refactorings from 10 Java projects | [2503.14340](https://arxiv.org/abs/2503.14340) | ✅ verbatim |
| Differential fuzzing: 19–35% of LLM refactorings functionally non-equivalent; ~21% of those undetected by existing test suites | [2602.15761](https://arxiv.org/abs/2602.15761) | ✅ verbatim (see §4 caveat) |
| Agentless: 32.00%, 96 correct fixes, $0.70/task on SWE-bench Lite | [2407.01489](https://arxiv.org/abs/2407.01489) | ✅ verbatim (v2; the 27.33%/$0.34 figures are v1) |
| TestGen-LLM: 75% built correctly, 57% passed reliably, 25% increased coverage; improved 11.5% of classes applied to; 73% of recommendations accepted for production | [2402.09171](https://arxiv.org/abs/2402.09171) | ✅ verbatim |
| Google migrations: 39 migrations, 3 developers, 12 months → 595 changes / 93,574 edits; 74.45% of changes and 69.46% of edits LLM-generated; ~50% time reduction | [2504.09691](https://arxiv.org/abs/2504.09691) | ✅ verbatim |
| SWE-bench: 32.67% solution leakage, 31.08% weak-test instances; SWE-Agent+GPT-4 drops 12.47% → 3.97% when filtered; >94% of issues predate model cutoffs | [2410.06992](https://arxiv.org/abs/2410.06992) | ✅ verbatim |
| Tangled refactoring: agents 21.43% vs humans 36.72%; intensity 0.66 vs 1.75; refactoring-aware refinement lifts compilability 19.34% → 38.33%, resolves +2.79% previously unresolved | [2605.22526](https://arxiv.org/abs/2605.22526) | ✅ verbatim (see §3c) |
| RefactorBench: 100 multi-file tasks; agents 22% vs human 87%; +43.9% from state conditioning | [2503.07832](https://arxiv.org/abs/2503.07832) | ✅ verbatim |
| Structural anchoring: +2.2pp Func@5, ~halved run-to-run variance, ~10% more input tokens | [2606.26979](https://arxiv.org/abs/2606.26979) | ✅ verbatim, but effect sizes are **small** — see §3d |
| Agentic refactoring prevalence: 26.1% of commits; Change Variable Type 11.8%, Rename Parameter 10.4%, Rename Variable 8.5%; maintainability 52.5%, readability 28.1% | [2511.04824](https://arxiv.org/abs/2511.04824) | ✅ verbatim |
| SWE-Refactor: 1,099 developer-written refactorings, 18 Java projects, validated by compilation + tests + refactoring detection | [2602.03712](https://arxiv.org/abs/2602.03712) | ✅ verbatim (922 atomic + 177 compound; Codex agent only 39.4% on compound) |
| GitClear 2025: 211M changed lines 2020–2024; 8× duplication increase; moved lines 24.1% → 9.5% | [GitClear 2025](https://www.gitclear.com/ai_assistant_code_quality_2025_research) | ✅ verbatim (vendor report — see §3e) |

**Minor discrepancy.** `research/` states the EM-Assist formative study covered "2,849 Extract Method scenarios"; the paper says 1,752. Immaterial to the conclusion.

---

## 2. New evidence not in `research/`

These post-date or were missed by the source documents and materially strengthen or reshape the case.

**SWE-ABS — independent corroboration of the ~20% figure.** ([2603.00520](https://arxiv.org/abs/2603.00520)) Rebuilt SWE-bench Verified's test suites via (1) coverage-driven augmentation using program slicing to target untested regions and (2) mutation-driven adversarial testing that synthesises plausible-but-incorrect patches. Result: **one in five "solved" patches from top-30 agents are semantically incorrect**, passing only because test suites are weak. Strengthens 50.2% of instances (25.1× prior work), rejects **19.71%** of previously passing patches, and drops the top agent from 78.80% → 62.20%.

> This is the most important single finding of the review. Two methodologically unrelated approaches — differential fuzzing on function-level refactorings, and adversarial test strengthening on repository-level patches — independently converge on **~20% of semantic breaks escaping existing test suites**. That convergence is the strongest empirical foundation Simpleton has. It also supplies a working blueprint for the test-strengthening rung of the verification stack.

**Foundation models as refactoring-correctness oracles.** ([2605.02096](https://arxiv.org/abs/2605.02096)) 226 real refactoring bugs collected over more than a decade from IntelliJ IDEA, Eclipse and NetBeans, spanning 47 refactoring types. Zero-shot detection accuracy: GPT-OSS-20B 80.5% (first run), GPT-5.4 93.8%; Gemini-3.1-Pro-Preview best overall, Gemma-4-31B best open-weight. Authors explicitly caution that metamorphic-testing robustness is *not* evidence against memorisation or contamination.

**Agent PR rejection economics.** ([2606.13468](https://arxiv.org/abs/2606.13468)) **46.41%** of fixes proposed by Copilot, Devin, Cursor and Claude in the AIDev dataset are rejected. Qualitative study of 306 non-merged PRs yields 14 reasons in four categories: incorrect implementation, CI/test failure, agent unable to implement, and **low priority** — i.e. correct work nobody wanted. Related: reviewer engagement is the strongest correlate of integration and larger diffs merge less often ([2602.19441](https://arxiv.org/abs/2602.19441)); acceptance is dominated by task type, with documentation at 82.1% vs new features at 66.1% across 7,156 PRs ([2602.08915](https://arxiv.org/abs/2602.08915)).

**METR RCT.** ([2507.09089](https://metr.org/blog/2025-07-10-early-2025-ai-experienced-os-dev-study/)) 16 experienced open-source developers, 246 tasks on mature repositories they averaged ~5 years of familiarity with: **19% slower** with AI tools available, while self-reporting a 20% speedup (and forecasting 24%). METR now labels the result historical.

**GitClear 2026 — updated and re-baselined.** ([The Maintainability Gap](https://www.gitclear.com/the_ai_code_quality_maintainability_gap)) 623M changes, 2023–2026. Block duplication +81% since 2023 (40.3 → 73.0 per million changed lines); commits containing a duplicated block up ~10× over two years; moved code 21% (2022) → 3.8% (YTD 2026); copy/paste 9.4% → 15.7%; **cross-file function calls −35%** (343 → 223 per thousand lines); legacy maintenance −74%; error-masking constructs +47%; two-week churn +15%.

**AtomicCommitBench.** ([2607.03332](https://arxiv.org/abs/2607.03332)) 800 real commit episodes across 10 Python projects. Agents achieve near-perfect replay validity (PPAR ≥ 0.988) but grouping quality of only **0.03–0.46 ARI** when decomposing squashed patches into coherent commits. Confirms a real capability gap behind the purity/tangling guard.

**RefAgent.** ([2511.03153](https://arxiv.org/abs/2511.03153)) Multi-agent, 8 Java projects: median 90% unit-test pass, median 52.5% code-smell reduction, and **median F1 79.15%** at identifying refactoring opportunities aligned with developer choices.

**Agent refactoring is annotation-dominated.** ([2601.20160](https://arxiv.org/abs/2601.20160)) Across 86 projects per group, the five most common agent refactoring types are all annotation-related, versus diverse structural improvements from humans. Cursor was the only agent showing a statistically significant increase in refactoring smells.

**Rust equivalence proofs.** REM2.0 extends the REM toolchain with automatic, annotation-free equivalence proofs between original and extracted functions ([2601.19207](https://arxiv.org/pdf/2601.19207)). Relevant prior art for the strongest rung of the verification ladder.

---

## 3. Corrections that change the design

### (a) "LLMs cannot identify refactoring opportunities" is true only unconstrained

The 28/180 and 7/180 figures are real. But the **same paper** ([2411.04444](https://arxiv.org/abs/2411.04444)) reports that explaining the expected refactoring subcategory and narrowing the search space in the prompt raised ChatGPT's success rate from **15.6% → 86.7%**, and that **63.6%** of its recommended solutions were comparable to or better than those built by human experts. (13 of 176 ChatGPT solutions and 9 of 137 Gemini solutions were unsafe — changing functionality or introducing syntax errors.)

Corroborating: RefAgent reaches median F1 79.15% at opportunity identification when given tools ([2511.03153](https://arxiv.org/abs/2511.03153)); CodeTaste finds that a **propose-then-implement decomposition improves alignment, and selecting the best-aligned proposal before implementation yields further gains** ([2603.04177](https://arxiv.org/abs/2603.04177)).

> **Design consequence.** Deterministic detection should *scope* the model, not *replace* it. The correct pipeline is: detector narrows the search space to a category and region → model proposes within that category → rank → apply. The original plan's "the LLM never invents opportunities; it triages detector output" is a weaker design than the evidence supports, and its Phase 0 exit criterion (maintainers agree ≥70% of top-10 backlog items are worth doing) measures the wrong thing — it evaluates a backlog product rather than a scoping mechanism.

The same paper also names the pattern: **RefactoringMirror**, a detect-and-reapply tactic that reapplies an LLM-identified refactoring using thoroughly tested refactoring engines. This is prior art for "codemod-first, LLM-second" and should be cited as such.

### (b) Deterministic refactoring engines are not a complete oracle

The original plan grants Tier 1 ("tool-safe refactorings") an oracle of "engine guarantees + tests." The 226-bug corpus in [2605.02096](https://arxiv.org/abs/2605.02096) — drawn from IntelliJ IDEA, Eclipse and NetBeans over a decade, across 47 refactoring types — shows that mature IDE refactoring engines introduce behavioural changes and compilation errors in production. "The IDE applied it" is evidence, not proof.

> **Design consequence.** Tier 1 needs a real oracle, not an assumed one. The same paper supplies a cheap one: zero-shot foundation-model bug detection at 80.5–93.8% accuracy, with no infrastructure and no refactoring-specific rules. It belongs in the verification stack between "tests pass" and "differential fuzzing."

### (c) Tangling is associated with compilability, not correctness

[2605.22526](https://arxiv.org/abs/2605.22526) is explicit: tangled refactorings are strongly associated with reduced compilability *"while exhibiting no significant association with functional correctness."*

> **Design consequence.** The purity/tangling guard is justified by build-greenness and reviewability. It must not be sold as a correctness control. The 19.34% → 38.33% compilability figure is the honest headline.

### (d) Structural anchoring effects are small

Verbatim but modest: +2.2pp Func@5, −1.6 interaction rounds, link-following rate 0.15–0.18 → 0.21–0.24, Pass@1 +3.4pp, at ~10% more input tokens. The paper's own framing is that anchoring works "less by making agents smarter and more by making their navigation disciplined and reproducible." `research/` describes this as "drastically improve," which overstates it. The reproducibility benefit (halved run-to-run variance) is the real value and is worth having; the capability benefit is marginal.

### (e) GitClear is a poor automatic control input

Directionally consistent across reports and the largest longitudinal dataset available — but vendor-produced, correlational, and **re-baselined between editions**. The 2025 report frames moved lines as 24.1% (2020) → 9.5% (2024); the 2026 report frames them as 21% (2022) → 3.8% (2026). Duplication moves from "8-fold increase" to "+81% since 2023 / ~10× more commits containing a duplicated block."

> **Design consequence.** Do not wire system autonomy to a vendor's shifting metric definitions. Compute the same *family* of signals in-house against frozen definitions. The 2026 edition contributes three signals worth adopting that the plan did not have: **cross-file function calls** (reuse/connectivity), **legacy maintenance rate**, and **error-masking construct density**.

---

## 4. Caveats on the strongest claim

The differential-fuzzing result is the empirical keystone, so its limits matter:

- **Function-level, not repository-level.** Evaluated on HumanEval/MBPP/APPS-derived data. Real codebases have side effects, I/O, global state and non-determinism that a differential fuzzer cannot reach. Expect coverage on real code to be a minority of changed functions.
- **Non-frontier models.** CodeLlama, Codestral, StarChat2, Qwen-2.5, Olmo-3, GPT-4o. The 19–35% band should not be assumed to hold for current frontier models.
- **Non-equivalence rises with complexity** (APPS 7–10 points worse than MBPP/HumanEval), so the direction of error is toward *worse* on real code even as model quality improves.

The SWE-ABS corroboration (§2) partially answers the first two objections — it is repository-level and evaluates current top-30 agents — which is why the convergence matters more than either result alone.

---

## 5. Claims that could not be verified

Treat as unsourced until traced. None are load-bearing after the redesign, but they should not be cited.

| Claim | Status |
|---|---|
| LM-CC yields "up to 20.9%" downstream task gains | Metric and direction confirmed ([2602.07882](https://arxiv.org/abs/2602.07882): classical metrics show no consistent correlation with LLM performance; semantics-preserving LM-CC reductions consistently improve downstream performance). **The 20.9% figure does not appear in the abstract.** |
| MiniCode / Librarian "2× better compression" | Paper not located |
| SWE-CI: 233-day average evolution, 71 consecutive commits, zero-regression rate <0.25 | Not located |
| ChainSWE: performance drops up to 70% at deeper chain positions | Not located |
| Google Ads 32→64-bit migration: "80% of code modifications in landed CLs fully AI-authored," 500M+-line codebase | Attributed to [2501.06972](https://arxiv.org/abs/2501.06972); the companion paper's 74.45%/69.46% figures are verified, this specific figure is not |
| RefactoringMiner ~99.8% precision / 95.8% recall | Widely cited, not independently checked here |

---

## 6. Competitive landscape — the plan's blind spot

`PLAN.md` (original) proposed a codemod-first transformation engine, a technical-debt backlog product, and a migration campaign engine without mentioning that all three are occupied.

**Moderne / OpenRewrite.** Batch-builds and serialises Lossless Semantic Trees so they are reused across repositories and teams without rebuilding, and exposes **OpenRewrite recipes as deterministic MCP tool calls to coding agents**, with pre-computed type-aware context and search (Prethink, Trigrep, local MCP server) — explicitly so agents "no longer need to invent upgrade paths." $30M Series B; Walmart, Allstate; embedded in AWS and Microsoft tooling. *This is the original plan's Stage 1 index plus Stage 4 preferred path, already shipping, for Java.* ([moderne.ai](https://moderne.ai/openrewrite), [FINOS](https://www.finos.org/blog/open-source-auto-refactoring-meets-ai-agent-to-modernize-fintech-software-at-scale))

**Codemod.com.** AI generates YAML ast-grep rules from natural-language descriptions; migration orchestration for organisation-wide change; plus "insights for identifying migration opportunities and monitoring technical debt" — i.e. the simplification-backlog product. ([codemod.com](https://codemod.com/))

**Grit.io.** AST-based transformations combined with ML, generating pull requests that clean up code and migrate frameworks; deterministic output suited to CI/CD. ([overview](https://www.brouseai.com/ai/grit-io))

**What is not occupied.** No product sells *"prove this diff is behaviour-preserving."* The research components exist — differential precondition checking, Mokav, EquiBench, REM2.0's annotation-free Rust equivalence proofs, SWE-ABS's test-strengthening pipeline, foundation-model oracles — but no one has integrated them into a gate that accepts an arbitrary diff and returns evidence.

---

## 7. Net implications for design

1. **The verification stack is the defensible artifact.** Strongest evidence in the corpus (§2 convergence), no incumbent (§6), and valuable on diffs from any author — which matters more each quarter as the share of agent-written code rises.
2. **Detectors scope the model; they do not replace it** (§3a). This changes what the detection layer emits and how it is evaluated.
3. **Every oracle is probabilistic, including the deterministic engines** (§3b). The ladder should be ordered by evidence strength and cost, with no rung treated as complete.
4. **Review throughput is the binding constraint, not generation** (§2: 46.41% rejection, "low priority" as a named category, larger diffs merging less, METR's 19%). Evidence packaging is the economic premise, not a nice-to-have.
5. **Renting the transformation layer removes the main reason to choose Java.** The deterministic-engine ecosystem was the argument for Java; if transformations are not being applied, what matters is fuzz/property-testing infrastructure and volume of agent-authored code. Java remains the right *evaluation* corpus (SWE-Refactor, the 226-bug oracle corpus, RefactoringMiner all target it).
6. **Public benchmarks should be refactoring-specific, not SWE-bench.** SWE-Refactor and CodeTaste both post-date most contamination and score with static checks plus tests — close to what the verification stack needs anyway.
